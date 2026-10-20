// Package gitlab talks to a self-hosted GitLab REST v4 instance and returns
// raw records (MRs, issues, commits). The events package maps these into
// NormalizedEvent; this package never produces NormalizedEvent directly.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
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
	return getAll[MergeRequest](ctx, c, fmt.Sprintf("/api/v4/projects/%s/merge_requests", url.PathEscape(projectPath)), q)
}

// FetchIssues returns issues updated within [since, now].
func (c *Client) FetchIssues(ctx context.Context, projectPath string, since time.Time) ([]Issue, error) {
	q := url.Values{}
	q.Set("updated_after", since.UTC().Format(time.RFC3339))
	q.Set("scope", "all")
	q.Set("per_page", "100")
	return getAll[Issue](ctx, c, fmt.Sprintf("/api/v4/projects/%s/issues", url.PathEscape(projectPath)), q)
}

// FetchCommits returns commits on the default branch authored within [since, now].
// ref_name= refs/heads/<default branch> would be tighter, but GitLab resolves
// the default branch server-side when ref_name is omitted, which is what we want.
func (c *Client) FetchCommits(ctx context.Context, projectPath string, since time.Time) ([]Commit, error) {
	q := url.Values{}
	q.Set("since", since.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	return getAll[Commit](ctx, c, fmt.Sprintf("/api/v4/projects/%s/repository/commits", url.PathEscape(projectPath)), q)
}

// getAll walks offset pagination until the response is empty. Honors 429
// Retry-After with exponential backoff up to maxRetries. Generic methods
// aren't allowed in Go, so this is a free function parameterized over the
// record type T.
const maxRetries = 3

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

// getPage issues one GET, applies 429-aware backoff, returns the page,
// whether more pages remain (X-Next-Page header), and any error.
func getPage[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, bool, error) {
	u := c.baseURL + path + "?" + q.Encode()
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
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt <= maxRetries:
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if err := sleep(ctx, wait); err != nil {
				return nil, false, err
			}
			continue
		case resp.StatusCode >= 500 && attempt <= maxRetries:
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, false, err
			}
			continue
		case resp.StatusCode >= 400:
			return nil, false, fmt.Errorf("gitlab %s %s: %d %s", http.MethodGet, path, resp.StatusCode, truncate(string(body), 200))
		}

		var batch []T
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, false, fmt.Errorf("decode %s: %w", path, err)
		}
		next := resp.Header.Get("X-Next-Page") != ""
		return batch, next, nil
	}
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return backoff(1)
	}
	if d, err := strconv.Atoi(h); err == nil {
		return time.Duration(d) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Until(t), 0)
	}
	return backoff(1)
}

func backoff(n int) time.Duration {
	d := time.Duration(1<<n) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
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