// Package gitlab talks to a self-hosted GitLab REST v4 instance and returns
// raw records (MRs, issues, commits). The events package maps these into
// NormalizedEvent; this package never produces NormalizedEvent directly.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	IID          int       `json:"iid"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	State        string    `json:"state"` // "opened" | "closed" | "merged"
	Author       User      `json:"author"`
	WebURL       string    `json:"web_url"`
	Labels       []string  `json:"labels"`
	UserNotesCount int     `json:"user_notes_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	MergedAt     *time.Time `json:"merged_at"`

	// Changes is populated by FetchMRChanges after the list fetch; nil
	// for MRs that haven't been detailed (or whose detail call failed).
	// The list endpoint doesn't expose per-file stats.
	Changes *ChangesSummary `json:"-"`
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
	IID          int       `json:"iid"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	Author       User      `json:"author"`
	WebURL       string    `json:"web_url"`
	Labels       []string  `json:"labels"`
	UserNotesCount int     `json:"user_notes_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ClosedAt     *time.Time `json:"closed_at"`
}

// Commit is the subset of GitLab's /repository/commits response shape we use.
type Commit struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"short_id"`
	Title        string    `json:"title"`
	AuthorName   string    `json:"author_name"`
	AuthorEmail  string    `json:"author_email"`
	WebURL       string    `json:"web_url"`
	CreatedAt    time.Time `json:"created_at"`
}

// User is the GitLab user summary embedded in MR/issue responses.
type User struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

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

// FetchMRChanges calls GET /merge_requests/:iid/changes and returns the
// per-MR aggregate (file count, total additions, total deletions). The
// per-file `diff` bodies in the payload are dropped by the JSON decoder —
// we declare only the fields we want, so the diff text never lands in
// memory or the prompt.
//
// One call per MR. Callers fetching N MRs should fan out concurrently to
// keep total wall time bounded; the pipeline does this with a worker pool.
func (c *Client) FetchMRChanges(ctx context.Context, projectPath string, iid int) (ChangesSummary, error) {
	var resp struct {
		Changes []struct {
			OldPath    string `json:"old_path"`
			NewPath    string `json:"new_path"`
			NewFile    bool   `json:"new_file"`
			DeletedFile bool  `json:"deleted_file"`
			RenamedFile bool  `json:"renamed_file"`
			Additions  int    `json:"additions"`
			Deletions  int    `json:"deletions"`
		} `json:"changes"`
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/changes", url.PathEscape(projectPath), iid)
	if err := c.getJSON(ctx, path, nil, &resp); err != nil {
		return ChangesSummary{}, err
	}
	s := ChangesSummary{Files: len(resp.Changes)}
	for _, f := range resp.Changes {
		s.Additions += f.Additions
		s.Deletions += f.Deletions
	}
	return s, nil
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
		resp.Body.Close()

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}