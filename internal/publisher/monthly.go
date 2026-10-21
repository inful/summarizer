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
	q := url.Values{}
	q.Set("per_page", "100")
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches?%s",
		g.Host, url.PathEscape(g.Project), q.Encode())

	body, err := g.get(ctx, urlStr)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	var branches []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &branches); err != nil {
		return nil, fmt.Errorf("decode branches: %w", err)
	}

	prefix := g.SourceBranchPrefix
	if prefix != "" {
		prefix += "/"
	}
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		if !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		if !isWeekSlug(strings.TrimPrefix(b.Name, prefix)) {
			continue
		}
		out = append(out, b.Name)
	}
	return out, nil
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

// get / getStatus: authenticated GETs against the GitLab API. Split so
// ReadReportFile can branch on 404 while the other callers don't care.
func (g *GitLabPublisher) get(ctx context.Context, urlStr string) ([]byte, error) {
	resp, err := g.getStatus(ctx, urlStr)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		return nil, fmt.Errorf("GET %s: %d %s", urlStr, resp.status, textutil.Truncate(string(resp.body), 300))
	}
	return resp.body, nil
}

type rawResponse struct {
	status int
	body   []byte
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return rawResponse{}, fmt.Errorf("read body: %w", err)
	}
	return rawResponse{status: resp.StatusCode, body: body}, nil
}

// ReadReportFile is on GitLabPublisher only. The interface deliberately
// does not include it: a monthly run against a local-disk publisher
// has nothing to read, and forcing every implementation to stub an
// unsupported method hides that. cmd/summarizer rejects a
// local-disk publisher before it ever gets here.
func (l *Local) ReadReportFile(context.Context, string, string) (string, error) {
	return "", errors.New("local-disk publisher has no reports to read; monthly rollups need publish.enabled with a GitLab target")
}
