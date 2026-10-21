// Package gitlab talks to a self-hosted GitLab REST v4 instance and returns
// raw records (MRs, issues, commits). The events package maps these into
// NormalizedEvent; this package never produces NormalizedEvent directly.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/inful/summarizer/internal/textutil"
)

// Client is a single GitLab REST v4 client. Construct with New.
type Client struct {
	baseURL string // e.g. "https://gitlab.example.com"
	token   string // PRIVATE-TOKEN value
	http    *http.Client
}

// New returns a client configured for the given host + PAT. timeout is the
// per-request timeout; 30s is a reasonable default for GitLab REST v4.
//
// baseURL may be either:
//   - the GitLab API root (e.g. "https://gitlab.example.com/api/v4"), or
//   - the GitLab web-UI base URL (e.g. "https://gitlab.example.com" or the
//     $CI_SERVER_URL CI variable). When the URL does not already end with
//     "/api/v4", New prepends it automatically — the same ergonomic fix
//     mreview applies for $CI_SERVER_URL-style configs.
func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: normalizeGitLabURL(baseURL),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

// normalizeGitLabURL accepts both web-UI base URLs and API-root URLs and
// returns the API root. A trailing slash is tolerated; the "/api/v4" suffix
// is idempotent (already-present URLs are passed through unchanged).
func normalizeGitLabURL(raw string) string {
	u := strings.TrimRight(raw, "/")
	if !strings.HasSuffix(u, "/api/v4") {
		u += "/api/v4"
	}
	return u
}

// MergeRequest is the subset of GitLab's /merge_requests response shape we use.
// json tags match GitLab's wire format; ignore fields we don't need.
type MergeRequest struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	State          string     `json:"state"` // "opened" | "closed" | "merged"
	Author         User       `json:"author"`
	WebURL         string     `json:"web_url"`
	Labels         []string   `json:"labels"`
	UserNotesCount int        `json:"user_notes_count"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	MergedAt       *time.Time `json:"merged_at"`

	// Changes is populated by FetchMRChanges after the list fetch; nil
	// for MRs that haven't been detailed (or whose detail call failed).
	// The list endpoint doesn't expose per-file stats.
	Changes *ChangesSummary `json:"-"`

	// DiffChunks is populated by FetchMRDetail when the operator opts
	// into --include-diffs. Each chunk's Diff field is capped at
	// maxDiffRunesPerFile to keep memory bounded even on multi-MB
	// refactors. Empty for MRs that haven't been detailed, or when the
	// feature is off.
	DiffChunks []DiffChunk `json:"-"`
}

// ChangesSummary is the per-MR aggregate of the GitLab /changes endpoint:
// number of files touched plus total line additions/deletions across them.
// The diff bodies themselves are discarded by the JSON decoder — only the
// aggregate survives, which keeps per-event prompt size bounded.
type ChangesSummary struct {
	Files     int
	Additions int
	Deletions int
}

// Issue is the subset of GitLab's /issues response shape we use.
type Issue struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	State          string     `json:"state"`
	Author         User       `json:"author"`
	WebURL         string     `json:"web_url"`
	Labels         []string   `json:"labels"`
	UserNotesCount int        `json:"user_notes_count"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
}

// Commit is the subset of GitLab's /repository/commits response shape we use.
type Commit struct {
	ID          string    `json:"id"`
	ShortID     string    `json:"short_id"`
	Title       string    `json:"title"`
	AuthorName  string    `json:"author_name"`
	AuthorEmail string    `json:"author_email"`
	WebURL      string    `json:"web_url"`
	CreatedAt   time.Time `json:"created_at"`
}

// User is the GitLab user summary embedded in MR/issue responses.
type User struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// Epic is the subset of GitLab's /groups/:id/epics response shape we use.
// Epics are a GitLab Premium/Ultimate feature; on Free-tier deployments
// the FetchEpics call returns (nil, nil) and the pipeline skips the
// epic section. See docs/deferred.md for the broader epic-rollup story.
type Epic struct {
	IID          int        `json:"iid"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	State        string     `json:"state"` // "opened" | "closed"
	Confidential bool       `json:"confidential"`
	WebURL       string     `json:"web_url"`
	StartDate    *Date      `json:"start_date"` // nullable; date-only "YYYY-MM-DD"
	DueDate      *Date      `json:"due_date"`   // nullable; date-only "YYYY-MM-DD"
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ClosedAt     *time.Time `json:"closed_at"` // nullable; RFC3339 datetime

	// Labels are the epic's own group labels, returned by
	// GET /groups/:id/epics as a plain string array (an array of
	// objects only when with_labels_details=true, which we don't ask
	// for). summarizer reads the `org::CUSTOMER_CODE` one to attribute
	// the epic to a customer — see policy.CustomerCode.
	Labels []string `json:"labels"`

	// GroupID is captured from the surrounding namespace context for
	// use by FetchEpicIssues (which is a group-scoped endpoint). The
	// /epics list endpoint doesn't return it explicitly; the caller
	// sets it after FetchEpics returns.
	GroupID int `json:"-"`

	// Issues is populated by FetchEpicIssues after the bare /epics list
	// fetch — the list endpoint only returns a count, not the linked
	// issues themselves. Map keeps this JSON-cleanable to a stub.
	Issues []Issue `json:"-"`
}

// Date wraps time.Time to decode GitLab's date-only fields (e.g.
// epic start_date / due_date) which arrive as "YYYY-MM-DD" rather
// than RFC3339. The wrapped value is at midnight UTC of that date.
type Date struct{ time.Time }

// UnmarshalJSON accepts the three shapes GitLab emits for date fields:
// null (absent), a JSON string in YYYY-MM-DD, or an RFC3339 datetime.
// The pointer-nil semantics come from the surrounding struct.
func (d *Date) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	// Strip quotes and try formats.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			d.Time = t
			return nil
		}
	}
	return fmt.Errorf("date: cannot parse %q as YYYY-MM-DD or RFC3339", s)
}

// Namespace is the parent of a GitLab project. Projects live under
// either a Group (kind="group") or a User (kind="user"). Epics only
// exist for group-scoped projects.
type Namespace struct {
	Kind     string `json:"kind"` // "group" | "user"
	FullPath string `json:"full_path"`
}

// ProjectSummary is the minimum info summarizer needs about a project
// during group discovery. The full Project payload (description,
// topics, avatar_url, ...) isn't useful here, so we model only the
// fields the discovery flow reads. Keep the type small — large
// responses on a big group can run into the megabytes.
type ProjectSummary struct {
	Path           string    `json:"path_with_namespace"`
	ID             int       `json:"id"`
	LastActivityAt time.Time `json:"last_activity_at"`
	// Topics is the project's GitLab topic list. summarizer reads
	// `summarize` and `summarize-team-only` from this list to
	// decide audience policy (see internal/policy.Classify). Free-
	// tier compatible.
	Topics []string `json:"topics"`
	// DefaultBranch is the project's HEAD ref (typically "main" or
	// "master"). Used by HasOptOutFile to read the project's
	// .summaryignore at the right ref. Empty when the API didn't
	// return the field on this payload shape — caller treats that
	// as "can't read opt-out file" and falls through to the
	// default.
	DefaultBranch string `json:"default_branch"`
}

// FetchGroupProjects lists projects in a single group. recursive=false
// is single-level (the GitLab default). recursive=true would traverse
// subgroups via /groups/:id/groups + recurse; that's a v2 follow-up
// — the parameter exists so the recursive future change is API-
// backwards-compatible.
func (c *Client) FetchGroupProjects(ctx context.Context, groupPath string, recursive bool) ([]ProjectSummary, error) {
	if recursive {
		// v1 ships single-level only. The branch exists so a future
		// recursive implementation can flip on without callers
		// needing to change their call sites.
		return nil, errors.New("FetchGroupProjects: recursive=true is not implemented; pass false")
	}
	q := url.Values{}
	q.Set("per_page", "100")
	path := fmt.Sprintf("/groups/%s/projects", url.PathEscape(groupPath))
	var out []ProjectSummary
	page := 1
	for {
		q.Set("page", strconv.Itoa(page))
		var batch []ProjectSummary
		var next bool
		err := c.doJSON(ctx, path, q, func(resp *http.Response, body []byte) error {
			next = resp.Header.Get("X-Next-Page") != ""
			return json.Unmarshal(body, &batch)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if !next || len(batch) == 0 {
			return out, nil
		}
		page++
	}
}

// FetchProjectsByTopic lists every project on the GitLab instance the
// token can see that carries ANY of the given topics. GitLab's ?topic=
// filter is AND-shaped (repeating the param narrows), so OR semantics
// come from one call per topic, unioned by project ID.
//
// This is the server-wide discovery primitive: it crosses group
// boundaries, so an operator running many groups doesn't have to
// enumerate them. Scope is bounded by the PAT — a token that can't see
// a project won't return it, and that's a permissions question, not a
// code bug.
//
// Results are stable: order follows the order of `topics`, with
// first-seen projects keeping their position across the union.
// FetchActiveProjects returns every project the token can see that had
// any activity at or after `since`. This is summarizer's default
// discovery: a project that was worked on runs without needing a topic
// or an entry in the config file.
//
// `last_activity_after` is the cheap server-side filter, so the result
// set is bounded by the period rather than by instance size. The
// caller still applies the per-project activity gate (MRs or issues in
// the window) because `last_activity_at` also moves for pushes, issue
// comments, and wiki edits that summarizer doesn't report on.
//
// Scope is bounded by the PAT, exactly as FetchProjectsByTopic is.
func (c *Client) FetchActiveProjects(ctx context.Context, since time.Time) ([]ProjectSummary, error) {
	q := url.Values{}
	q.Set("last_activity_after", since.UTC().Format(time.RFC3339))
	q.Set("order_by", "last_activity_at")
	q.Set("per_page", "100")
	projs, truncated, err := c.pagedProjects(ctx, "/projects", q, maxActiveScanPages)
	if err != nil {
		return nil, fmt.Errorf("discover active: %w", err)
	}
	if truncated {
		fmt.Fprintf(os.Stderr, "summarizer: activity scan stopped after %d pages (%d projects); "+
			"more active projects exist and were not examined\n", maxActiveScanPages, len(projs))
	}
	return projs, nil
}

// pagedProjects is getAll with a page ceiling. Returns what it found
// plus whether there was more, so the caller can say so rather than
// silently presenting a truncated scan as complete.
func (c *Client) pagedProjects(ctx context.Context, path string, q url.Values, maxPages int) ([]ProjectSummary, bool, error) {
	var out []ProjectSummary
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		batch, next, err := getPage[ProjectSummary](ctx, c, path, q)
		if err != nil {
			return out, false, err
		}
		out = append(out, batch...)
		if !next || len(batch) == 0 {
			return out, false, nil
		}
	}
	return out, true, nil
}

// maxActiveScanPages bounds how far FetchActiveProjects will page.
// The endpoint has no server-side limit, so on an instance where the
// token can see thousands of projects this would walk every page and
// hold every ProjectSummary in memory before the caller ranked and
// capped them — and the first page is already the most recently active
// hundred, because we ask for order_by=last_activity_at.
//
// shortcut: a run that hits this has not seen every active project.
// Raising it costs one page per 100 projects; the caller logs how many
// candidates were examined, so a truncation is visible in the run
// output.
const maxActiveScanPages = 10

func (c *Client) FetchProjectsByTopic(ctx context.Context, topics []string) ([]ProjectSummary, error) {
	seen := make(map[int]bool)
	out := make([]ProjectSummary, 0)
	for _, topic := range topics {
		q := url.Values{}
		q.Set("topic", topic)
		q.Set("per_page", "100")
		page := 1
		for {
			q.Set("page", strconv.Itoa(page))
			var batch []ProjectSummary
			var next bool
			err := c.doJSON(ctx, "/projects", q, func(resp *http.Response, body []byte) error {
				next = resp.Header.Get("X-Next-Page") != ""
				return json.Unmarshal(body, &batch)
			})
			if err != nil {
				return nil, fmt.Errorf("discover topic %s: %w", topic, err)
			}
			for _, p := range batch {
				if seen[p.ID] {
					continue
				}
				seen[p.ID] = true
				out = append(out, p)
			}
			if !next || len(batch) == 0 {
				break
			}
			page++
		}
	}
	return out, nil
}

// ProbeProjectActivity asks one cheap question per project: did this
// project have any MR, issue, or commit updated in [since, now]?
// Returns (true, nil) as soon as any endpoint returns a row,
// (false, nil) when all three come back empty.
//
// 404 on any endpoint (project moved, renamed, never existed, or
// just confidential) soft-degrades to (false, nil) so the discovery
// loop skips it without surfacing transport noise. 403, 429 and 5xx
// are real problems and hard-error: a 403 is a token or permissions
// issue the operator must see, a 429 is a rate-limit, a 5xx is
// infrastructure. Soft-degrading any of these would silently hide a
// project from the report.
//
// Why all three, in this order:
//   - MRs are gitlab's "engineering work landed" signal, and they
//     answer most of the time.
//   - Issues are the "this team is alive" signal: a triage week
//     produces no MRs but a project with five closed issues is not
//     quiet.
//   - Commits are the "someone worked here without opening anything"
//     signal. A repo maintained by direct pushes to the default
//     branch produces neither an MR nor an issue, so without this
//     probe the whole week's work on such a repo is invisible —
//     exactly the repos that the activity-default discovery is most
//     likely to surface.
//
// Cost is at most two extra /<kind>?per_page=1 calls per discovered
// project, and only for projects already quiet on everything probed
// before them.
//
// (Inlined HTTP loop rather than reusing doJSON: we need a 404
// soft-degrade path, and doJSON's 4xx switch returns a hard error
// before the decode callback runs. A future refactor could add a
// soft-fail mode to doJSON itself, but the surface area is small.)
func (c *Client) ProbeProjectActivity(ctx context.Context, projectPath string, since time.Time) (bool, error) {
	for _, kind := range []string{"merge_requests", "issues", "repository/commits"} {
		active, err := c.probeHasAny(ctx, projectPath, kind, since)
		if err != nil || active {
			return active, err
		}
	}
	return false, nil
}

// probeHasAny is one side of the activity gate: did the project have
// any record of the given kind updated in [since, now]? The shared
// shape lets ProbeProjectActivity compose two of them cheaply.
func (c *Client) probeHasAny(ctx context.Context, projectPath, kind string, since time.Time) (bool, error) {
	u := c.baseURL + fmt.Sprintf("/projects/%s/%s", url.PathEscape(projectPath), kind) +
		"?updated_after=" + url.QueryEscape(since.UTC().Format(time.RFC3339)) +
		"&scope=all&per_page=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return false, fmt.Errorf("gitlab %s: 429 (probe is one-shot, no retry)", http.MethodGet)
	case resp.StatusCode >= 500:
		return false, fmt.Errorf("gitlab %s: %d (probe)", http.MethodGet, resp.StatusCode)
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("gitlab %s: %d %s",
			http.MethodGet, resp.StatusCode, truncate(string(body), 200))
	}
	var batch json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&batch); err != nil {
		return false, err
	}
	// Both endpoints return an array. We don't need the fields,
	// only the count — decoding into RawMessage and trimming the
	// outer brackets avoids per-field allocation.
	raw := string(batch)
	return strings.HasPrefix(raw, "[") && strings.TrimSpace(raw) != "[]", nil
}

// FetchMRs returns MRs updated within [since, now]. updated_after is the
// correct filter for "activity in window": it catches both newly opened and
// FetchMRs returns MRs updated within [since, now]. updated_after is the
// correct filter for "activity in window": it catches both newly opened and
// newly commented-on MRs without scanning the full project history.
func (c *Client) FetchMRs(ctx context.Context, projectPath string, since time.Time) ([]MergeRequest, error) {
	q := url.Values{}
	q.Set("updated_after", since.UTC().Format(time.RFC3339))
	q.Set("scope", "all")
	q.Set("per_page", "100")
	return getAll[MergeRequest](ctx, c, fmt.Sprintf("/projects/%s/merge_requests", url.PathEscape(projectPath)), q)
}

// FetchEpics returns epics for a group. Epics are a Premium/Ultimate
// feature; on Free-tier deployments (or when the group simply doesn't
// have any) this returns (nil, nil) so the caller can skip the epic
// section without surfacing an error. 401 (auth) DOES hard-error: a
// valid token is still load-bearing.
//
// Note: this endpoint handles 403/404 specially (soft-degrade), so it
// does NOT use the shared getPage helper. The free-tier 403 is a
// feature, not a bug, and the shared switch can't distinguish them.
func (c *Client) FetchEpics(ctx context.Context, groupPath string) ([]Epic, error) {
	path := fmt.Sprintf("/groups/%s/epics", url.PathEscape(groupPath))
	var out []Epic
	for page := 1; ; page++ {
		batch, next, err := c.pageEpics(ctx, path, page)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if !next {
			return out, nil
		}
	}
}

// pageEpics issues one page of /epics. Distinct from getPage because
// it carries the soft-degrade contract for 403/404.
// nolint:unused // (returns are used via the wrapped call path; signature is identical to getPage)
func (c *Client) pageEpics(ctx context.Context, path string, page int) ([]Epic, bool, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("page", strconv.Itoa(page))
	u := c.baseURL + path + "?" + q.Encode()
	var batch []Epic
	var next bool
	var attempt int
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("PRIVATE-TOKEN", c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, false, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return nil, false, err
			}
			continue
		case resp.StatusCode >= 500 && attempt <= maxRetries:
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, false, err
			}
			continue
		case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
			// Soft-degrade: Free-tier or wrong group path. Caller treats
			// (nil, nil) as "no epic data; skip the section".
			return nil, false, nil
		case resp.StatusCode >= 400:
			return nil, false, fmt.Errorf("gitlab %s %s: %d %s", http.MethodGet, path, resp.StatusCode, truncate(string(body), 200))
		}
		next = resp.Header.Get("X-Next-Page") != ""
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, false, err
		}
		return batch, next, nil
	}
}

// FetchEpicIssues returns the issues linked to a single epic. The bare
// /epics list endpoint only returns a count, so we hit this per epic
// to know which issues each contains. Same graceful-degrade contract
// as FetchEpics: 403/404 → (nil, nil); 401 → real error.
func (c *Client) FetchEpicIssues(ctx context.Context, groupPath string, epicIID int) ([]Issue, error) {
	path := fmt.Sprintf("/groups/%s/epics/%d/issues", url.PathEscape(groupPath), epicIID)
	q := url.Values{}
	q.Set("per_page", "100")
	var out []Issue
	page := 1
	for {
		q.Set("page", strconv.Itoa(page))
		var batch []Issue
		var next bool
		u := c.baseURL + path + "?" + q.Encode()
		var attempt int
		var decodeErr error
		for {
			attempt++
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("PRIVATE-TOKEN", c.token)
			req.Header.Set("Accept", "application/json")
			resp, err := c.http.Do(req)
			if err != nil {
				return nil, err
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
				if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
					return nil, err
				}
				continue
			case resp.StatusCode >= 500 && attempt <= maxRetries:
				if err := sleep(ctx, backoff(attempt)); err != nil {
					return nil, err
				}
				continue
			case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound:
				return nil, nil
			case resp.StatusCode >= 400:
				return nil, fmt.Errorf("gitlab %s %s: %d %s", http.MethodGet, path, resp.StatusCode, truncate(string(body), 200))
			}
			next = resp.Header.Get("X-Next-Page") != ""
			decodeErr = json.Unmarshal(body, &batch)
			break
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		out = append(out, batch...)
		if !next || len(batch) == 0 {
			return out, nil
		}
		page++
	}
}

// FetchProjectNamespace returns the parent namespace (group or user)
// of a project. The pipeline uses this to derive the group's full path
// for /epics calls — projects don't carry their own epic endpoint,
// only groups do.
func (c *Client) FetchProjectNamespace(ctx context.Context, projectPath string) (Namespace, error) {
	var pj struct {
		Namespace Namespace `json:"namespace"`
	}
	err := c.getJSON(ctx, fmt.Sprintf("/projects/%s", url.PathEscape(projectPath)), nil, &pj)
	return pj.Namespace, err
}

// FetchProjectSummary returns the fields summarizer needs about a single
// project: path, last-activity timestamp, and topic list. Called by
// the D5 audience classifier (see internal/policy) so it can decide
// whether the project should produce customer-facing output. The
// 404-soft-degrade contract (returns nil summary, no error) lets the
// caller treat a moved/renamed project as "no metadata available"
// rather than failing the run — the classifier's default-safe behavior
// then keeps the project on the team-only side until the operator can
// re-tag.
func (c *Client) FetchProjectSummary(ctx context.Context, projectPath string) (*ProjectSummary, error) {
	var ps ProjectSummary
	var attempt int
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.baseURL+"/projects/"+url.PathEscape(projectPath), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, nil
		case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode >= 500 && attempt <= maxRetries:
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode >= 400:
			return nil, fmt.Errorf("gitlab %s /projects/%s: %d %s",
				http.MethodGet, projectPath, resp.StatusCode, truncate(string(body), 200))
		}
		if err := json.Unmarshal(body, &ps); err != nil {
			return nil, fmt.Errorf("decode /projects/%s: %w", projectPath, err)
		}
		return &ps, nil
	}
}

// HasOptOutFile reports whether the project carries either of the
// "summariser must skip me" sentinel files at its repo root:
//
//	.summaryignore    — summariser-specific opt-out
//	.docignore        — docbuilder convention, also honoured here
//
// Either file's presence is sufficient to opt the project out of the
// run entirely. The project-owner's opt-out is checked at the
// pipeline layer (not the policy classifier) because the file is
// content of a specific ref, and the policy classifier stays a pure
// function over topics + override list.
//
// Returns:
//
//	(true,  nil) — at least one of the sentinel files exists on the
//	              default branch; exclude the project.
//	(false, nil) — neither file exists; include the project.
//	(_, err)     — transport / auth / 5xx; surface to the caller.
//
// The contract matches FetchProjectSummary's 404/403 handling:
// missing or inaccessible files do NOT error out the run, they
// simply don't opt the project out. A 401, 429 or 5xx DOES error,
// because that's an auth/rate/infra problem the operator must see.
func (c *Client) HasOptOutFile(ctx context.Context, projectPath string) (bool, error) {
	for _, name := range optOutFileNames {
		exists, err := c.hasRepoRootFile(ctx, projectPath, name)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// optOutFileNames is the list of sentinel files that opt a project out
// of the run. The order is "summariser's own first, docbuilder alias
// second" — HasOptOutFile short-circuits on the first match, so the
// summariser's own file is checked first and the public alias second.
var optOutFileNames = []string{".summaryignore", ".docignore"}

// hasRepoRootFile GETs the project's default-branch tree at a file
// name and reports whether the file exists. 200 ⇒ exists, 404 ⇒
// doesn't, 401/429/5xx ⇒ real error. Anything else (e.g. 403 for
// confidential projects the PAT can't read) is treated as "doesn't
// exist" so a project that can't be read is treated as opted-in
// rather than crashing the run — same call we make elsewhere.
func (c *Client) hasRepoRootFile(ctx context.Context, projectPath, name string) (bool, error) {
	// Read default branch + project path in one call (cheap), then
	// hit the Files API. We need the default branch name to query
	// the file; the Files endpoint requires ?ref=<ref>.
	summary, err := c.FetchProjectSummary(ctx, projectPath)
	if err != nil {
		return false, err
	}
	if summary == nil {
		// 404 from /projects/:id — treat as "no opt-out file" so the
		// caller doesn't fail the run on a moved/renamed project.
		return false, nil
	}
	ref := summary.DefaultBranch
	if ref == "" {
		// Old GitLab versions may not return default_branch on every
		// payload shape. Without a ref we can't query the file;
		// fall back to "no opt-out".
		return false, nil
	}

	u := c.baseURL + fmt.Sprintf("/projects/%s/repository/files/%s?ref=%s",
		url.PathEscape(projectPath), url.PathEscape(name), url.QueryEscape(ref))
	var attempt int
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("PRIVATE-TOKEN", c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return false, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return false, nil
		case resp.StatusCode == http.StatusOK:
			return true, nil
		case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return false, err
			}
			continue
		case resp.StatusCode >= 500 && attempt <= maxRetries:
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return false, err
			}
			continue
		case resp.StatusCode == http.StatusForbidden:
			// Confidential project or a PAT the operator can read
			// the topic/metadata of but not the file. The
			// project-owner signal can't reach us, so fall through
			// to "no opt-out" — the run continues, and if the
			// file truly exists the operator will see the
			// published report and can iterate.
			return false, nil
		case resp.StatusCode >= 400:
			return false, fmt.Errorf("gitlab %s /repository/files/%s: %d %s",
				http.MethodGet, name, resp.StatusCode, truncate(string(body), 200))
		}
	}
}

// FetchIssues returns issues updated within [since, now].
func (c *Client) FetchIssues(ctx context.Context, projectPath string, since time.Time) ([]Issue, error) {
	q := url.Values{}
	q.Set("updated_after", since.UTC().Format(time.RFC3339))
	q.Set("scope", "all")
	q.Set("per_page", "100")
	return getAll[Issue](ctx, c, fmt.Sprintf("/projects/%s/issues", url.PathEscape(projectPath)), q)
}

// FetchCommits returns commits on the default branch authored within [since, now].
// ref_name= refs/heads/<default branch> would be tighter, but GitLab resolves
// the default branch server-side when ref_name is omitted, which is what we want.
func (c *Client) FetchCommits(ctx context.Context, projectPath string, since time.Time) ([]Commit, error) {
	q := url.Values{}
	q.Set("since", since.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	return getAll[Commit](ctx, c, fmt.Sprintf("/projects/%s/repository/commits", url.PathEscape(projectPath)), q)
}

// cappedDiff is a diff body that truncates during JSON decoding rather
// than after it.
//
// The naive version decodes `Diff` into a plain string and truncates
// afterwards. That allocates the full multi-megabyte body first and
// throws it away — so a vendor-bump MR with a 40 MB diff cost 40 MB
// of string allocation per MR, times changeConcurrency concurrent
// workers, on every run including the default --include-diffs=off one
// that never uses the text.
//
// encoding/json hands a custom Unmarshaler a SUBSLICE of its input
// buffer (d.data[start:end]), so nothing is copied: we convert at most
// maxDiffRunesPer runes and the rest of the value is simply never
// materialised as a string.
type cappedDiff struct {
	Text string
}

func (c *cappedDiff) UnmarshalJSON(b []byte) error {
	// b is the raw JSON string literal, still escaped.
	s, err := strconv.Unquote(string(b))
	if err != nil {
		// Not a string (null, or something unexpected). Treat as empty
		// rather than failing the whole MR: a missing diff body is
		// survivable, an unparseable one is not worth losing the
		// aggregate over.
		c.Text = ""
		return nil
	}
	if r := []rune(s); len(r) > maxDiffRunesPerFile {
		s = string(r[:maxDiffRunesPerFile]) + "...[truncated]"
	}
	c.Text = s
	return nil
}

// FetchMRDetail calls GET /merge_requests/:iid/changes once and returns
// BOTH the per-MR aggregate (file count, total additions/deletions) and
// the per-file diff chunks, with each chunk's Diff capped at
// maxDiffRunesPerFile.
//
// One call, two consumers: the aggregate feeds the "+N/-M in K files"
// suffix on every prompt line; the chunks feed the team prompt when the
// operator passes --include-diffs. An earlier design had two methods
// (FetchMRChanges / FetchMRDetail) and the pipeline called BOTH, which
// meant two identical authenticated round-trips and two full JSON
// decodes per MR.
//
// One call per MR. Callers fetching N MRs should fan out concurrently
// to keep total wall time bounded; the pipeline does this with a worker
// pool.
//
// shortcut: the HTTP response body is still buffered whole by getJSON
// before decoding, so peak memory is ~1x the /changes payload per
// in-flight MR rather than being bounded outright. Streaming the decode
// is the upgrade if a real MR ever ships a multi-GB diff; at 4-way
// concurrency a 40 MB payload is ~160 MB, which is survivable.
func (c *Client) FetchMRDetail(ctx context.Context, projectPath string, iid int) (ChangesSummary, []DiffChunk, error) {
	chunks, err := c.fetchMRChangesRaw(ctx, projectPath, iid)
	if err != nil {
		return ChangesSummary{}, nil, err
	}
	return summarizeChunks(chunks), chunks, nil
}

// fetchMRChangesRaw hits /changes once and returns the per-file chunks
// with diff bodies in the wire payload. The diff field is always
// decoded — GitLab puts it in the same payload, so skipping it would
// mean a second decode of the same body. FetchMRDetail truncates it
// immediately afterwards.
func (c *Client) fetchMRChangesRaw(ctx context.Context, projectPath string, iid int) ([]DiffChunk, error) {
	var resp struct {
		Changes []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			NewFile     bool   `json:"new_file"`
			DeletedFile bool   `json:"deleted_file"`
			RenamedFile bool   `json:"renamed_file"`
			Additions   int    `json:"additions"`
			Deletions   int    `json:"deletions"`
			Diff        cappedDiff
		} `json:"changes"`
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/changes", url.PathEscape(projectPath), iid)
	if err := c.getJSON(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	chunks := make([]DiffChunk, 0, len(resp.Changes))
	for _, f := range resp.Changes {
		chunks = append(chunks, DiffChunk{
			Path:      filePath(f.OldPath, f.NewPath),
			Additions: f.Additions,
			Deletions: f.Deletions,
			Diff:      f.Diff.Text,
		})
	}
	return chunks, nil
}

// maxDiffRunesPerFile caps the per-file diff body retained in memory
// after decoding. GitLab can return multi-MB diffs for big refactors;
// we don't need all of it to summarize — pick a value that's big enough
// to capture intent (function signature + first hunk) and small enough
// to keep per-MR memory bounded. The prompt formatter applies
// additional per-MR and total caps.
const maxDiffRunesPerFile = 600

// DiffChunk is one file's worth of /changes data. Diff is the raw text and
// is bounded at decode time by maxDiffRunesPerFile.
type DiffChunk struct {
	Path      string // display path: old→new for renames, otherwise the current path
	Additions int
	Deletions int
	Diff      string // truncated at maxDiffRunesPerFile
}

// filePath picks the path to display for a /changes entry: the new path
// unless the file was deleted, in which case the old path. For renames,
// joins "old → new" so the engineer scanning the report can tell at a
// glance that a file moved.
func filePath(oldPath, newPath string) string {
	switch {
	case oldPath == newPath:
		return newPath
	case newPath == "":
		return oldPath
	case oldPath == "":
		return newPath
	default:
		return oldPath + " → " + newPath
	}
}

// summarizeChunks aggregates per-file stats. Caller is responsible for
// any truncation applied to the chunks before this.
func summarizeChunks(chunks []DiffChunk) ChangesSummary {
	s := ChangesSummary{Files: len(chunks)}
	for _, c := range chunks {
		s.Additions += c.Additions
		s.Deletions += c.Deletions
	}
	return s
}

// getAll walks offset pagination until the response is empty. Honors 429
// Retry-After with exponential backoff up to maxRetries. Generic methods
// aren't allowed in Go, so this is a free function parameterized over the
// record type T.
const maxRetries = 3

// retryAfterCapDefault is the upper bound on a server-requested Retry-After
// wait. A misbehaving server that returns "Retry-After: 999999" gets capped.
// Matches mreview's default.
const retryAfterCapDefault = 60 * time.Second

// maxBackoff caps the exponential backoff between retries. Also prevents
// `1<<n` from overflowing at large attempt counts.
const maxBackoff = 30 * time.Second

func getAll[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	var out []T
	page := 1
	for {
		q.Set("page", strconv.Itoa(page))
		batch, next, err := getPage[T](ctx, c, path, q)
		if err != nil {
			return out, err
		}
		out = append(out, batch...)
		if !next || len(batch) == 0 {
			return out, nil
		}
		page++
	}
}

// doJSON issues one GET against baseURL+path, with the same retry/backoff
// policy as getPage, then hands the response + body to decode. The decode
// callback also receives the response so callers can read headers (X-Next-Page
// for pagination, Link for relative URLs, etc.).
func (c *Client) doJSON(ctx context.Context, path string, q url.Values, decode func(resp *http.Response, body []byte) error) error {
	u := c.baseURL + path
	if q != nil {
		u += "?" + q.Encode()
	}
	var attempt int
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("PRIVATE-TOKEN", c.token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if err := sleep(ctx, wait); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 500 && attempt <= maxRetries:
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 400:
			return fmt.Errorf("gitlab %s %s: %d %s", http.MethodGet, path, resp.StatusCode, truncate(string(body), 200))
		}
		return decode(resp, body)
	}
}

// getJSON is the convenience wrapper for endpoints that return a single
// object (no pagination). The JSON decoder ignores fields we don't model,
// so a /changes response with full per-file diff bodies in the payload
// costs us only the bytes-on-the-wire — diff text is dropped on the floor.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, into any) error {
	return c.doJSON(ctx, path, q, func(_ *http.Response, body []byte) error {
		return json.Unmarshal(body, into)
	})
}

// getPage issues one GET, applies 429-aware backoff, returns the page,
// whether more pages remain (X-Next-Page header), and any error.
func getPage[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, bool, error) {
	var batch []T
	var next bool
	err := c.doJSON(ctx, path, q, func(resp *http.Response, body []byte) error {
		next = resp.Header.Get("X-Next-Page") != ""
		return json.Unmarshal(body, &batch)
	})
	if err != nil {
		return nil, false, err
	}
	return batch, next, nil
}

// retryAfter parses the Retry-After header value, which may be either a
// delta-seconds integer ("120") or an HTTP-date. Returns the requested
// wait, capped at retryAfterCapDefault, or falls back to the exponential
// backoff when the header is missing or unparseable.
func retryAfter(h string) time.Duration {
	if h == "" {
		return backoff(1)
	}
	var d time.Duration
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = max(time.Until(t), 0)
	} else {
		return backoff(1)
	}
	if d > retryAfterCapDefault {
		d = retryAfterCapDefault
	}
	return d
}

// backoff returns the wait before the next attempt: base * 2^(n-1), capped
// at maxBackoff, with ±20% jitter to avoid thundering-herd retries.
//
// attempt is 1-indexed (the wait BEFORE the Nth retry), so attempt=1 yields
// ~base, attempt=2 yields ~2*base, etc.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	mult := 1 << (n - 1) // n=1 → 1, n=2 → 2, n=3 → 4, ...
	if mult > 30 {
		mult = 30
	}
	d := time.Duration(mult) * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	// ±20% jitter.
	jitter := time.Duration(float64(d) * 0.2 * (2*rand.Float64() - 1))
	d += jitter
	if d < 0 {
		d = 0
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string { return textutil.Truncate(s, n) }
