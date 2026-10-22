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
	"slices"
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
	IID       int        `json:"iid"`
	Title     string     `json:"title"`
	State     string     `json:"state"` // "opened" | "closed"
	WebURL    string     `json:"web_url"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"` // nullable; RFC3339 datetime

	// Confidential is GitLab's own "do not show this to people who
	// shouldn't see it" flag. It is honoured on the customer epic
	// rollup — see policy.EpicCustomerCodes' callers — because those
	// files are published public: true.
	Confidential bool `json:"confidential"`

	// Labels are the epic's own group labels, returned by
	// GET /groups/:id/epics as a plain string array (an array of
	// objects only when with_labels_details=true, which we don't ask
	// for). summarizer reads the `org::CUSTOMER_CODE` one to attribute
	// the epic to a customer — see policy.CustomerCode.
	Labels []string `json:"labels"`

	// Issues is populated by FetchEpicIssues after the bare /epics list
	// fetch — the list endpoint only returns a count, not the linked
	// issues themselves. Map keeps this JSON-cleanable to a stub.
	Issues []Issue `json:"-"`
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
	projs, truncated, err := getAll[ProjectSummary](ctx, c, "/projects", q, maxActiveScanPages)
	if err != nil {
		return nil, fmt.Errorf("discover active: %w", err)
	}
	if truncated {
		fmt.Fprintf(os.Stderr, "summarizer: activity scan stopped after %d pages (%d projects); "+
			"more active projects exist and were not examined\n", maxActiveScanPages, len(projs))
	}
	return projs, nil
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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
	out, _, err := getAll[MergeRequest](ctx, c,
		fmt.Sprintf("/projects/%s/merge_requests", url.PathEscape(projectPath)), q, 0)
	return out, err
}

// FetchEpics returns epics for a group. Epics are a Premium/Ultimate
// feature; on Free-tier deployments (or when the group simply doesn't
// have any) this returns (nil, nil) so the caller can skip the epic
// section without surfacing an error. 401 (auth) DOES hard-error: a
// valid token is still load-bearing.
//
// Note: this endpoint handles 403/404 specially (soft-degrade), so it
func (c *Client) FetchEpics(ctx context.Context, groupPath string) ([]Epic, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	// 403/404 soft-degrade: epics are Premium, so a Free-tier instance
	// or a wrong group path must skip the section, not fail the run.
	out, _, err := getAll[Epic](ctx, c,
		fmt.Sprintf("/groups/%s/epics", url.PathEscape(groupPath)),
		q, 0, http.StatusForbidden, http.StatusNotFound)
	return out, err
}

// FetchEpicIssues returns the issues linked to a single epic. The bare
// /epics list endpoint only returns a count, so we hit this per epic
// to know which issues each contains. Same graceful-degrade contract
// as FetchEpics: 403/404 → (nil, nil); 401 → real error.
func (c *Client) FetchEpicIssues(ctx context.Context, groupPath string, epicIID int) ([]Issue, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	// 403/404 soft-degrade: an epic the token cannot see, or one on a
	// plan without the feature, yields no issues rather than an error,
	// so the report simply shows an epic with no child tasks.
	out, _, err := getAll[Issue](ctx, c,
		fmt.Sprintf("/groups/%s/epics/%d/issues", url.PathEscape(groupPath), epicIID),
		q, 0, http.StatusForbidden, http.StatusNotFound)
	return out, err
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
// FetchProjectSummary GETs one project. 404 soft-degrades to (nil, nil):
// a renamed or moved project must not fail a run that is otherwise fine.
func (c *Client) FetchProjectSummary(ctx context.Context, projectPath string) (*ProjectSummary, error) {
	var ps ProjectSummary
	decoded := false
	err := c.doJSON(ctx, fmt.Sprintf("/projects/%s", url.PathEscape(projectPath)), nil,
		func(_ *http.Response, body []byte) error {
			decoded = true
			return json.Unmarshal(body, &ps)
		}, http.StatusNotFound)
	if err != nil {
		return nil, err
	}
	if !decoded {
		return nil, nil
	}
	return &ps, nil
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
	// The default branch is resolved ONCE, above the filename loop. The
	// lookup used to live inside hasRepoRootFile, so checking one
	// project cost two project fetches and two file reads. On a
	// 50-project discovery run that is 100 requests spent re-reading a
	// field the /projects listing had already delivered.
	summary, err := c.FetchProjectSummary(ctx, projectPath)
	if err != nil {
		return false, err
	}
	if summary == nil || summary.DefaultBranch == "" {
		// 404 (moved or renamed), or an old GitLab that omits
		// default_branch. Either way the file cannot be queried, so
		// treat the project as opted in rather than failing the run.
		return false, nil
	}
	for _, name := range optOutFileNames {
		exists, err := c.hasRepoRootFile(ctx, projectPath, summary.DefaultBranch, name)
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
// hasRepoRootFile reports whether one file exists at the repo root.
// ref is the project's default branch, which the Files endpoint
// requires. 404 and 403 mean "not there": 403 is a confidential
// project, or a PAT that can read metadata but not files, and the
// project-owner's opt-out signal cannot reach us in that case — so the
// run continues rather than crashing. 401/429/5xx are real errors.
func (c *Client) hasRepoRootFile(ctx context.Context, projectPath, ref, name string) (bool, error) {
	found := false
	err := c.doJSON(ctx,
		fmt.Sprintf("/projects/%s/repository/files/%s",
			url.PathEscape(projectPath), url.PathEscape(name)),
		url.Values{"ref": {ref}},
		func(resp *http.Response, _ []byte) error {
			// Reached only on a 2xx. A soft status returns before the
			// decoder runs, so found stays false without this flag.
			found = resp.StatusCode == http.StatusOK
			return nil
		}, http.StatusNotFound, http.StatusForbidden)
	if err != nil {
		return false, err
	}
	return found, nil
}

// FetchIssues returns issues updated within [since, now].
func (c *Client) FetchIssues(ctx context.Context, projectPath string, since time.Time) ([]Issue, error) {
	q := url.Values{}
	q.Set("updated_after", since.UTC().Format(time.RFC3339))
	q.Set("scope", "all")
	q.Set("per_page", "100")
	out, _, err := getAll[Issue](ctx, c,
		fmt.Sprintf("/projects/%s/issues", url.PathEscape(projectPath)), q, 0)
	return out, err
}

// FetchCommits returns commits on the default branch authored within [since, now].
// ref_name= refs/heads/<default branch> would be tighter, but GitLab resolves
// the default branch server-side when ref_name is omitted, which is what we want.
func (c *Client) FetchCommits(ctx context.Context, projectPath string, since time.Time) ([]Commit, error) {
	q := url.Values{}
	q.Set("since", since.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	out, _, err := getAll[Commit](ctx, c,
		fmt.Sprintf("/projects/%s/repository/commits", url.PathEscape(projectPath)), q, 0)
	return out, err
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

// maxResponseBytes caps how much of a response body is read into memory.
// Every read below goes through io.LimitReader rather than io.ReadAll:
// an unbounded read is a remote-triggered OOM, and /merge_requests/:iid/
// changes is the one that gets large — a single merge request that
// committed a generated lockfile or a vendored binary returns tens of
// megabytes, and fillMRDetail runs four of those concurrently. The
// truncated diff still parses; a diff body that is genuinely larger
// than this is not something a weekly summary needs anyway.
const maxResponseBytes = 16 << 20

// maxBackoff caps the exponential backoff between retries. Also prevents
// `1<<n` from overflowing at large attempt counts.
const maxBackoff = 30 * time.Second

// getAll walks offset pagination until the response is empty. maxPages
// bounds the walk: 0 means unlimited, which is right for the per-project
// endpoints where the result IS the answer, and wrong for the
// instance-wide scan where only the first few pages matter. Reports
// whether it stopped early so the caller can say so rather than
// presenting a truncated scan as complete.
func getAll[T any](ctx context.Context, c *Client, path string, q url.Values, maxPages int, soft ...int) ([]T, bool, error) {
	var out []T
	for page := 1; maxPages == 0 || page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		batch, next, err := getPage[T](ctx, c, path, q, soft...)
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

// doJSON issues one GET against baseURL+path, with the same retry/backoff
// policy as getPage, then hands the response + body to decode. The decode
// callback also receives the response so callers can read headers (X-Next-Page
// for pagination, Link for relative URLs, etc.).
// soft lists statuses that should decode as an empty result rather than
// an error. GitLab answers 403 and 404 for endpoints a plan or a token
// does not reach (epics on Free tier, a project the PAT cannot see) —
// "not available" rather than "something broke", so a run continues
// without the section instead of aborting.
func (c *Client) doJSON(ctx context.Context, path string, q url.Values, decode func(resp *http.Response, body []byte) error, soft ...int) error {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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
		case slices.Contains(soft, resp.StatusCode):
			return nil
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
func getPage[T any](ctx context.Context, c *Client, path string, q url.Values, soft ...int) ([]T, bool, error) {
	var batch []T
	var next bool
	err := c.doJSON(ctx, path, q, func(resp *http.Response, body []byte) error {
		next = resp.Header.Get("X-Next-Page") != ""
		return json.Unmarshal(body, &batch)
	}, soft...)
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
