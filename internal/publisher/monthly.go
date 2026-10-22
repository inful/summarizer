package publisher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/inful/summarizer/internal/textutil"
)

// ListPeriodBranches returns the source branches this publisher created
// for weekly runs — "summarizer/2026-W42" and friends — in the order
// GitLab returns them.
//
// The filter is deliberately narrow: the branch must start with the
// configured SourceBranchPrefix AND end in a weekly slug matching
// YYYY-Wnn. That keeps out the target's own default branch, branches
// from other tools, and a person's feature work, none of which are
// summarizer's weekly rollups.
//
// Used by `summarizer monthly`, which rolls up last month's weeks.
func (g *GitLabPublisher) ListPeriodBranches(ctx context.Context) ([]string, error) {
	// Paged. A reports repo accrues one branch per week and never loses
	// them, so a single per_page=100 stops returning recent weeks in the
	// second year — and the monthly rollup then quietly reports those
	// weeks as "no rollup file" and still exits zero. The monthly
	// branches are filtered out below, but the server has already spent
	// their slots by then, costing 12 a year.
	prefix := g.SourceBranchPrefix
	if prefix != "" {
		prefix += "/"
	}
	out := make([]string, 0, 64)
	for page := 1; page <= maxBranchPages; page++ {
		q := url.Values{}
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		body, next, err := g.getPaged(ctx, fmt.Sprintf("/projects/%s/repository/branches",
			url.PathEscape(g.Project)), q)
		if err != nil {
			return nil, fmt.Errorf("list branches: %w", err)
		}
		var branches []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &branches); err != nil {
			return nil, fmt.Errorf("decode branches: %w", err)
		}
		for _, b := range branches {
			if !strings.HasPrefix(b.Name, prefix) {
				continue
			}
			if !isWeekSlug(strings.TrimPrefix(b.Name, prefix)) {
				continue
			}
			out = append(out, b.Name)
		}
		if !next || len(branches) == 0 {
			return out, nil
		}
	}
	fmt.Fprintf(os.Stderr, "summarizer: branch listing stopped after %d pages; older or newer weeks may be missing\n", maxBranchPages)
	return out, nil
}

// maxBranchPages is 200 branches at 100 a page — twenty years of weekly
// runs, well past any repo's lifetime.
const maxBranchPages = 200

// getPaged is get plus the X-Next-Page header, so a caller can walk a
// paginated list. Mirrors internal/gitlab's getPage without importing
// that package's unexported helper.
func (g *GitLabPublisher) getPaged(ctx context.Context, path string, q url.Values) ([]byte, bool, error) {
	u := fmt.Sprintf("%s/api/v4%s?%s", g.Host, path, q.Encode())
	resp, err := g.getStatus(ctx, u)
	if err != nil {
		return nil, false, err
	}
	if resp.status >= 400 {
		return nil, false, fmt.Errorf("GET %s: %d %s", u, resp.status, textutil.Truncate(string(resp.body), 300))
	}
	return resp.body, resp.headers.Get("X-Next-Page") != "", nil
}

// isWeekSlug reports whether s looks like "2026-W42" — four digits, a
// dash, a W, and exactly two digits. Deliberately not a full calendar
// check: a branch named 2026-W99 is malformed input we should still
// list, and the monthly command's week→date mapping will skip it.
func isWeekSlug(s string) bool {
	if len(s) != len("2026-W42") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 4:
			if c != '-' {
				return false
			}
		case 5:
			if c != 'W' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// ReadReportFile GETs one file off a source branch and returns its
// decoded body. A missing file yields ("", nil): a week whose rollup
// was never published, or was deleted by hand, should skip that week
// rather than fail the whole monthly rollup.
//
// Any other 4xx/5xx is a real error — the operator needs to know the
// token can't read the repo, as opposed to the file simply not being
// there.
func (g *GitLabPublisher) ReadReportFile(ctx context.Context, branch, path string) (string, error) {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s?ref=%s",
		g.Host, url.PathEscape(g.Project), pathEscape(path), url.QueryEscape(branch))

	resp, err := g.getStatus(ctx, urlStr)
	if err != nil {
		return "", err
	}
	switch {
	case resp.status == http.StatusNotFound:
		return "", nil
	case resp.status >= 400:
		return "", fmt.Errorf("read %s on %s: %d %s",
			path, branch, resp.status, textutil.Truncate(string(resp.body), 200))
	}

	var fr fileResponse
	if err := json.Unmarshal(resp.body, &fr); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	if strings.EqualFold(fr.Encoding, "base64") {
		decoded, err := base64.StdEncoding.DecodeString(fr.Content)
		if err != nil {
			return "", fmt.Errorf("decode base64 %s: %w", path, err)
		}
		return string(decoded), nil
	}
	return fr.Content, nil
}

// getStatus is the authenticated GET every read here goes through.
// getPaged and ReadReportFile both branch on the status themselves, so
// there is no error-returning wrapper to keep.

type rawResponse struct {
	status  int
	body    []byte
	headers http.Header
}

func (g *GitLabPublisher) getStatus(ctx context.Context, urlStr string) (rawResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return rawResponse{}, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return rawResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded, like every other body read in this repo.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return rawResponse{}, fmt.Errorf("read body: %w", err)
	}
	return rawResponse{status: resp.StatusCode, body: body, headers: resp.Header}, nil
}

// DefaultRef implements run.MonthlyReader. The target repo's default
// branch is where a merged week's rollup lives once its source branch
// has been deleted.
func (g *GitLabPublisher) DefaultRef() string { return g.DefaultBranch }

// ReadReportFile is on GitLabPublisher only. The interface deliberately
// does not include it: a monthly run against a local-disk publisher
// has nothing to read, and forcing every implementation to stub an
// unsupported method hides that. cmd/summarizer rejects a
// local-disk publisher before it ever gets here.
// DefaultRef is empty on Local: there is no default branch, and
// ReadReportFile below rejects the whole local-disk case anyway.
func (l *Local) DefaultRef() string { return "" }

func (l *Local) ReadReportFile(context.Context, string, string) (string, error) {
	return "", errors.New("local-disk publisher has no reports to read; monthly rollups need publish.enabled with a GitLab target")
}
