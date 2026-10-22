package publisher

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/inful/mdfm"
	"github.com/inful/summarizer/internal/textutil"
)

// Retry policy for the write path, matching internal/gitlab's reads and
// internal/llm's: a transient upstream failure must not lose an
// unattended Sunday-night run, and a hostile Retry-After must not park
// the process for a day. Not shared — the gitlab package's are
// unexported, and a third copy of four small functions is worse than
// two.
//
// shortcut: identical values to internal/gitlab and internal/llm. If
// one changes, this is the third place to change it; promote to a
// shared package only when a fourth copy appears.
const (
	maxRetries           = 3
	retryAfterCapDefault = 60 * time.Second
	maxBackoff           = 30 * time.Second
)

// maxResponseBytes bounds every body read here, for the same reason
// internal/llm does: this talks to a host, and an unbounded read is a
// remote-triggered OOM.
const maxResponseBytes = 16 << 20

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
	return min(d, retryAfterCapDefault)
}

func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return min(time.Duration(1<<(n-1))*time.Second, maxBackoff)
}

// sleep waits for d, or returns early when ctx is cancelled — which is
// what makes Ctrl-C interrupt a backoff instead of waiting it out.
// Overwritten in tests so the retry path is exercised without real
// waits; see sleepFn.
var sleepFn = sleep

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
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

// GitLabPublisher commits each report to a per-period source branch on
// the target project and opens/updates a merge request so operators
// (or project owners) can review and amend before publishing.
//
// Typical flow per period (e.g. "2026-10-19"):
//
//  1. POST  /projects/:id/repository/branches  (summarizer/2026-W42, from main) — once per period
//  2. PUT   /projects/:id/repository/files/:path?branch=summarizer/2026-W42 — once per file
//  3. GET   /projects/:id/merge_requests?source_branch=summarizer/2026-W42
//     - none: POST .../merge_requests
//     - open:  leave as-is; the new commit stacks on the existing MR
//     - merged / closed: refuse to re-open; the error names the state
//
// Idempotency: re-runs for the same period accumulate commits on the
// same MR. Each re-run only changes the files for that audience — old
// files are untouched (Files API is content-addressed).
type GitLabPublisher struct {
	Host               string // e.g. "https://gitlab.example.com"
	Token              string // PAT with api scope on the target project
	Project            string // path "group/subgroup/project"
	Prefix             string // path prefix for reports, e.g. "reports" ("" = repo root)
	DefaultBranch      string // typically "main"; MR target
	SourceBranchPrefix string // e.g. "summarizer"; full branch is "<prefix>/<period>"
	HTTP               *http.Client

	// Per-branch memo, so a run that publishes a hundred files onto one
	// branch asks GitLab about that branch once. Four of every six
	// requests in Publish were branchExists plus three mergeRequests
	// calls, all returning the same answer for every file.
	//
	// Guarded because Publisher is documented as concurrency-safe.
	// Keyed by branch, so it cannot go stale: a new period is a new
	// branch name, and the CLI builds a fresh publisher per process.
	mu    sync.Mutex
	cache map[string]branchFacts
}

// branchFacts is what one branch lookup established: whether it existed,
// and the MR URL if it had one.
type branchFacts struct {
	exists bool
	// mrURL is cached only on a POSITIVE outcome — an MR was found, or
	// one was created. A miss is never cached, so the first file of a
	// fresh period queries and creates, and the rest reuse the URL. That
	// asymmetry is deliberate: caching "there is none" would make the
	// second file create a second MR.
	mrURL string
	// closed records that the period's MR is merged or closed, so the
	// branch will never merge again. Worth caching: a re-run of an
	// already-published week would otherwise pay three queries per file
	// to rediscover it.
	closed bool
}

// branchKnown reports whether this branch was already confirmed to
// exist earlier in the run.
func (g *GitLabPublisher) branchKnown(branch string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cache[branch].exists
}

// remember records a fact about a branch for the rest of the run.
func (g *GitLabPublisher) remember(branch string, f branchFacts) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cache == nil {
		g.cache = map[string]branchFacts{}
	}
	prev := g.cache[branch]
	// Never let a later partial observation erase what we know.
	if !f.exists {
		f.exists = prev.exists
	}
	if f.mrURL == "" {
		f.mrURL = prev.mrURL
	}
	if !f.closed {
		f.closed = prev.closed
	}
	g.cache[branch] = f
}

// recalled returns what is already known about a branch.
func (g *GitLabPublisher) recalled(branch string) branchFacts {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cache[branch]
}

// NewGitLab constructs a publisher. host + token + project are required;
// prefix and DefaultBranch have sensible defaults.
func NewGitLab(host, token, project, prefix, defaultBranch, sourceBranchPrefix string) *GitLabPublisher {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return &GitLabPublisher{
		Host:               strings.TrimRight(host, "/"),
		Token:              token,
		Project:            project,
		Prefix:             strings.Trim(pathPrefix(prefix), "/"),
		DefaultBranch:      defaultBranch,
		SourceBranchPrefix: sourceBranchPrefix,
		HTTP:               &http.Client{Timeout: 60 * time.Second},
	}
}

// PeriodBranch is the deterministic source-branch name for one period.
// Exposed so future cron jobs / merge scripts can find the same branch
// the publisher used.
func (g *GitLabPublisher) PeriodBranch(period string) string {
	if g.SourceBranchPrefix == "" {
		return period
	}
	return g.SourceBranchPrefix + "/" + period
}

// targetPath builds the repo-relative file path for one report, with a
// per-period subdirectory so each week's reports stay grouped together.
// After every week's MR is merged into the default branch, the target
// repo's main tree looks like:
//
//	reports/2026-W42/  group-proj--wk--customer.md
//	                    group-proj--wk--team.md
//	                    combined--rollup--wk--customer.md
//	                    combined--rollup--wk--team.md
//	reports/2026-W43/  ...
//	reports/2026-09/   monthly--2026-09--customer.md   (monthly rollup)
//
// Browsable week-by-week in the GitLab UI; docbuilder indexes recursively
// so all weeks are reachable from the search.
func (g *GitLabPublisher) targetPath(period, name string) string {
	if g.Prefix == "" {
		return period + "/" + name
	}
	return g.Prefix + "/" + period + "/" + name
}

// pathEscape calls url.PathEscape (/) → (%2F); GitLab's Files API takes
// a path component in the URL, so separators must be percent-encoded.
func pathEscape(s string) string {
	return url.PathEscape(s)
}

// pathPrefix returns the input trimmed; helper for the constructor's
// shrugging-default handling.
func pathPrefix(s string) string {
	return strings.TrimSpace(s)
}

// API request/response shapes.

type branchReq struct {
	// Branch is the new branch name; Ref is the branch name or
	// commit SHA to fork from. GitLab's POST /repository/branches
	// expects both as required fields; the field is `ref`, not
	// `start_point` (which was the legacy name before GitLab 12.2 —
	// modern instances 400 with "ref is missing, ref is empty"
	// when the body uses the old name).
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
}

type fileReq struct {
	Branch        string `json:"branch"`
	CommitMessage string `json:"commit_message"`
	Content       string `json:"content"`  // base64
	Encoding      string `json:"encoding"` // "base64"
	// LastCommitID is the SHA of the commit that last touched the file
	// on the target branch. Omitted on first publish (file doesn't
	// exist yet, so no SHA to round-trip). When the file already
	// exists on the branch — every re-run in monday-bucket mode —
	// GitLab's PUT returns 400 "A file with this name doesn't
	// exist" if the field is absent. The misleading message
	// actually means "this is an update, you must authorise it".
	LastCommitID string `json:"last_commit_id,omitempty"`
}

type mrReq struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	RemoveSourceBranch bool   `json:"remove_source_branch,omitempty"`
}

type mergeRequest struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
}

// Publish conforms to Publisher. Body is wrapped with docbuilder frontmatter
// (via the pipeline's own frontmatter build; here we accept it verbatim
// — frontmatter generation lives in the pipeline so the publisher stays
// filesystem-agnostic).
func (g *GitLabPublisher) Publish(ctx context.Context, req Request) (Result, error) {
	if g.Host == "" || g.Token == "" || g.Project == "" {
		return Result{}, errors.New("gitlab publisher: host, token, and project are required")
	}
	if req.Period == "" {
		return Result{}, errors.New("gitlab publisher: req.Period is required (used as source-branch slug)")
	}

	branch := g.PeriodBranch(req.Period)
	path := g.targetPath(req.Period, req.Name)

	// Read-before-write: preserve the existing docbuilder UID (when the
	// file is already on this branch from an earlier run) so the
	// cross-project link identity (the docbuilder invariant) survives
	// re-runs. When the file is absent — fresh period or first publish —
	// existingUID is "" and NewFrontmatter will mint via mdid. The
	// lastCommitID returned alongside is what the PUT body needs to
	// authorise an update; for a new file it's "" and `omitempty`
	// drops it.
	existingUID, lastCommitID, err := g.readExistingUID(ctx, path, branch)
	if err != nil {
		return Result{}, fmt.Errorf("read existing %s@%s: %w", path, branch, err)
	}

	// Wrap the body with docbuilder-format frontmatter. The fingerprint
	// in the frontmatter is the SHA-256 of the raw body via mdfp, so even
	// if the title/date change between runs, a content-equivalent re-run
	// is detectable by docbuilder. Public is set per Request — customer
	// reports get public: true so docbuilder exposes them externally;
	// team reports omit the key (internal-only).
	fm := NewFrontmatter(req.Project, req.Audience, req.Period, req.Kind, time.Now().UTC(), req.Body, existingUID)
	fm.Public = req.Public
	finalBody := fm.Render(req.Body)
	encoded := base64.StdEncoding.EncodeToString(finalBody)

	// 1. Ensure the per-period source branch exists.
	if err := g.ensureBranch(ctx, branch); err != nil {
		return Result{}, fmt.Errorf("ensure branch %s: %w", branch, err)
	}

	// 2. Create-or-update the file. GitLab's API has two endpoints
	// with different semantics:
	//   - POST /repository/files creates a new file. 400 with
	//     "You are attempting to create a file that already exists"
	//     if the path is taken.
	//   - PUT  /repository/files updates an existing file. 400 with
	//     "A file with this name doesn't exist" if the path is free
	//     — and in older versions (e.g. before the UpdateService got
	//     its create_commit! fallback) the same error fires for the
	//     create case. Modern GitLab auto-creates on PUT, but
	//     self-hosted instances running the older API code reject
	//     with this message either way.
	//
	// The reliable signal is lastCommitID from the read: empty means
	// the file does not exist on the branch, non-empty means it does.
	// Use that to choose the right method instead of trying to
	// decode the error.
	//
	// The body is the same for both; last_commit_id is `omitempty`,
	// so it drops out of the POST body automatically.
	var method, urlStr string
	if lastCommitID == "" {
		method = "POST"
	} else {
		method = "PUT"
	}
	urlStr = fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s",
		g.Host, url.PathEscape(g.Project), pathEscape(path))
	putBody, _ := json.Marshal(fileReq{
		Branch:        branch,
		CommitMessage: commitMessage(req),
		Content:       encoded,
		Encoding:      "base64",
		LastCommitID:  lastCommitID,
	})
	if err := g.api(ctx, urlStr, method, putBody, nil, true); err != nil {
		// The response body rarely tells you what's wrong with the
		// request body. Echo the request fields so the next debug
		// session doesn't need tcpdump.
		return Result{}, fmt.Errorf("%s file %s@%s: %w (body: branch=%s last_commit_id=%q commit_message=%q encoding=%s content_bytes=%d)",
			method, path, branch, err, branch, lastCommitID, commitMessage(req), "base64", len(encoded))
	}

	// 3. Find or create the MR for this period.
	mrURL, err := g.findOrCreateMR(ctx, branch, req)
	if err != nil {
		if errors.Is(err, errPeriodClosed) {
			// The week was already published and its MR merged or
			// closed. Re-running it is the ordinary response to a
			// report that said the wrong thing, and the file has
			// just been committed to a branch that will never merge
			// again. That is worth a line, not a failed project: the
			// caller skips both cross-project rollups and exits
			// non-zero when Publish returns an error.
			fmt.Fprintf(os.Stderr, "summarizer: %s was already published (%v); %s was updated on %s and will not reach %s\n",
				req.Period, err, req.Name, branch, g.DefaultBranch)
			return Result{
				Ref:    fmt.Sprintf("%s/-/blob/%s/%s", g.Host, branch, path),
				Closed: true,
			}, nil
		}
		// File landed; an MR-side failure should still report the branch ref.
		return Result{Ref: fmt.Sprintf("%s/-/blob/%s/%s", g.Host, branch, path)}, err
	}
	return Result{
		Ref:   fmt.Sprintf("%s/-/blob/%s/%s", g.Host, branch, path),
		MRURL: mrURL,
	}, nil
}

// ensureBranch creates the per-period source branch if it's missing.
// GET-first: GitLab 400s with "has already been taken" on a duplicate
// branch name, and one run publishes many files into one period — so
// the naive POST-every-time version died on the second report.
func (g *GitLabPublisher) ensureBranch(ctx context.Context, branch string) error {
	if g.branchKnown(branch) {
		return nil
	}
	if g.branchExists(ctx, branch) {
		// Remember it. Without this the memo only pays off on the
		// first run of a period, where this file creates the branch —
		// and every re-run, restart or second `summarizer run` of the
		// same week re-fetched per file, which is the case an operator
		// hits while iterating on a report.
		g.remember(branch, branchFacts{exists: true})
		return nil
	}
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches",
		g.Host, url.PathEscape(g.Project))
	body, _ := json.Marshal(branchReq{
		Branch: branch,
		Ref:    g.DefaultBranch,
	})
	if err := g.api(ctx, urlStr, "POST", body, nil, true); err != nil {
		return err
	}
	g.remember(branch, branchFacts{exists: true})
	return nil
}

// branchExists treats only 200 as "yes"; every other status (404, 403,
// transport) falls through to the create attempt, which surfaces the
// real error rather than swallowing it here.
func (g *GitLabPublisher) branchExists(ctx context.Context, branch string) bool {
	g.mu.Lock()
	known := g.cache[branch].exists
	g.mu.Unlock()
	if known {
		return true
	}
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches/%s",
		g.Host, url.PathEscape(g.Project), pathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return false
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// fileResponse is the subset of the Files API GET response we care about.
// Documentation: GET /projects/:id/repository/files/:file_path.
// GitLab 404s when the file doesn't exist on that ref — that's our
// "first publish for this period" signal.
type fileResponse struct {
	Encoding     string `json:"encoding"` // "text" or "base64"
	Content      string `json:"content"`
	LastCommitID string `json:"last_commit_id"` // SHA of the commit that last touched this file; required for the round-trip PUT
}

// readExistingUID GETs the file at (path, branch) and parses out the
// docbuilder `uid` frontmatter key, plus the commit SHA of the file
// on that branch. The two are returned together because they're
// populated by the same GET response and used by the same PUT —
// splitting them across two methods would mean two round-trips to
// the Files API for no reason.
//
// Three returns:
//
//   - ("", "", nil) when the file is absent (first publish for this period).
//   - (uid, sha, nil) when the file exists and carries a uid.
//   - ("", "", err) on transport / parse failure — the caller surfaces it.
//
// Cross-run stability of the UID comes from this read; the mint itself
// is non-deterministic (UUID v7). The SHA is what GitLab's PUT needs
// to authorise an update on a file that already exists on the branch
// — see fileReq.LastCommitID for why.
func (g *GitLabPublisher) readExistingUID(ctx context.Context, path, branch string) (uid, lastCommitID string, err error) {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s?ref=%s",
		g.Host, url.PathEscape(g.Project), pathEscape(path), url.QueryEscape(branch))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// First publish for this file → caller will mint a fresh uid
		// and the PUT will create the file (no last_commit_id needed).
		return "", "", nil
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return "", "", fmt.Errorf("%s %s: %d %s",
			http.MethodGet, urlStr, resp.StatusCode, textutil.Truncate(string(body), 300))
	}

	var fr fileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return "", "", fmt.Errorf("decode %s: %w", urlStr, err)
	}
	// GitLab returns text files with encoding="text" verbatim and
	// binary-ish files with encoding="base64". Frontmatter is text, so
	// we hit "text" in practice — handle base64 too for safety.
	content := fr.Content
	if strings.EqualFold(fr.Encoding, "base64") {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return "", "", fmt.Errorf("decode base64 content: %w", err)
		}
		content = string(decoded)
	}

	doc, err := mdfm.ParseString(content)
	if err != nil {
		// Malformed frontmatter (e.g. file body contains only a partial
		// copy or the YAML isn't a mapping). Surface the parse error so
		// the operator notices on stderr; the upstream caller treats
		// empty-uid as "mint fresh".
		return "", "", fmt.Errorf("parse existing %s: %w", path, err)
	}
	if !doc.HasFrontmatter() {
		// The UID is docbuilder's cross-project link identity — the
		// whole reason for read-before-write. Losing it silently
		// breaks every /_uid/<uid>/ link to the document and nothing
		// says so on a run that otherwise reports success.
		warnUIDLost(path)
		return "", fr.LastCommitID, nil
	}
	parsedUID, found, err := doc.GetString("uid")
	if err != nil {
		return "", "", fmt.Errorf("extract uid from %s: %w", path, err)
	}
	if !found {
		// An existing summarizer-written file should always carry a uid
		// (NewFrontmatter renders one on every Publish). Absence means
		// either a manual edit, a corrupted clone, or a file from a
		// tool other than summarizer. The next publish mints fresh,
		// which is the recoverable half; the broken links are not, so
		// say so.
		warnUIDLost(path)
		return "", fr.LastCommitID, nil
	}
	return parsedUID, fr.LastCommitID, nil
}

// warnUIDLost names the file whose uid is about to be replaced. The
// write still proceeds — a missing uid is not a reason to refuse, and
// refusing would wedge the week — but the link loss is permanent, so
// the operator gets a line that points at the file they can restore.
func warnUIDLost(path string) {
	fmt.Fprintf(os.Stderr, "summarizer: %s has no uid in its frontmatter; minting a new one. "+
		"Any docbuilder /_uid/ links to the old document are now broken — restore the uid line "+
		"if this file was edited by hand.\n", path)
}

// mergeRequests lists MRs; we use a wide state filter so we can detect
// both "none" and "already merged/closed" cleanly.
func (g *GitLabPublisher) mergeRequests(ctx context.Context, sourceBranch, state string) ([]mergeRequest, error) {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?source_branch=%s&state=%s",
		g.Host, url.PathEscape(g.Project), url.QueryEscape(sourceBranch), url.QueryEscape(state))
	var out []mergeRequest
	if err := g.api(ctx, urlStr, "GET", nil, &out, true); err != nil {
		return nil, err
	}
	return out, nil
}

// errPeriodClosed says the week's MR already reached merged or closed,
// so this branch will never be merged again. It is a sentinel so
// Publish can treat re-publishing a completed week as a logged no-op
// rather than a failed project.
var errPeriodClosed = errors.New("period already published")

// findOrCreateMR opens the MR if one isn't open for this source branch.
// Returns the MR web URL. If an MR exists in merged / closed state we
// decline to re-open it and return an error naming that state; the
// caller surfaces it.
func (g *GitLabPublisher) findOrCreateMR(ctx context.Context, branch string, req Request) (string, error) {
	// One run publishes every file onto one branch, and all four of
	// these queries answer the same question for all of them. Resolved
	// once, then reused.
	//
	// "closed" has to come back as the SENTINEL, not as ("", nil).
	// Publish sets Result.Closed only on the errPeriodClosed path, and
	// printSummary counts from that — so returning a bare success here
	// made every file after the first count as published, and the run
	// reported "100 file(s) written (1 already published this week)"
	// when nothing had reached main. The queries are still saved.
	if f := g.recalled(branch); f.closed {
		return "", fmt.Errorf("%w: %s", errPeriodClosed, req.Period)
	} else if f.mrURL != "" {
		return f.mrURL, nil
	}

	mrs, err := g.mergeRequests(ctx, branch, "opened")
	if err != nil {
		return "", err
	}
	if len(mrs) > 0 {
		g.remember(branch, branchFacts{mrURL: mrs[0].WebURL})
		return mrs[0].WebURL, nil
	}

	// No open MR. Closing an MR without merging is the ordinary way to
	// abandon a week, so `closed` counts too — querying only `merged`
	// let that case open a SECOND MR for the same branch.
	nonOpen, err := g.mergeRequests(ctx, branch, "merged")
	if err != nil {
		return "", err
	}
	closedMRs, err := g.mergeRequests(ctx, branch, "closed")
	if err != nil {
		return "", err
	}
	all := append(nonOpen, closedMRs...)
	if len(all) > 0 {
		g.remember(branch, branchFacts{closed: true})
		return "", fmt.Errorf("%w: %s is %s", errPeriodClosed, req.Period, all[0].State)
	}

	// No MR at all — create one.
	createURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests",
		g.Host, url.PathEscape(g.Project))
	body, _ := json.Marshal(mrReq{
		SourceBranch: branch,
		TargetBranch: g.DefaultBranch,
		Title:        fmt.Sprintf("Summarizer rollup %s", req.Period),
		Description: fmt.Sprintf(
			"Automated weekly rollup generated by summarizer.\n\n"+
				"- **Period:** %s\n%s"+
				"- **Visibility:** %s\n\n"+
				"Files in this branch have docbuilder-format frontmatter.\n\n"+
				"Merge to publish.",
			req.Period, projectLine(req), visibilityLine(req)),
	})
	var mr mergeRequest
	if err := g.api(ctx, createURL, "POST", body, &mr, false); err != nil {
		// Deliberately NOT remembered: the next file must be free to
		// try again, since we do not know whether the POST landed.
		return "", err
	}
	g.remember(branch, branchFacts{mrURL: mr.WebURL})
	return mr.WebURL, nil
}

// visibilityLine renders who can see the published files. The previous
// version printed req.Kind here under an "- **Audience:**" heading, so
// every MR read "Audience: rollup" — a category, not an audience.
func visibilityLine(req Request) string {
	if req.Public {
		return "customer-facing (public: true in frontmatter)"
	}
	return "internal"
}

// projectLine renders the "Project(s)" row of the MR body. A per-project
// file names its project; a rollup does not — printing req.Name there
// put "combined--epics-acme--wk--customer.md" under a heading that says
// projects, which is what a reviewer reads before merging.
func projectLine(req Request) string {
	if req.Project != "" {
		return "- **Project(s):** " + req.Project + "\n"
	}
	return ""
}

func commitMessage(req Request) string {
	if req.Kind == KindRollup {
		return fmt.Sprintf("summarizer: rollup %s for %s", req.Period, req.Name)
	}
	return fmt.Sprintf("summarizer: %s for %s", req.Period, req.Name)
}

// api issues method URL with body (or nil for GET) and decodes the
// response into out (or discards). Handles auth + JSON content-type for
// write paths.
//
// retry says whether a 429 or 5xx is worth another attempt. It is safe
// for every call here EXCEPT the merge-request create: GitLab does not
// de-duplicate those, so a retried POST that actually succeeded the
// first time opens two MRs for one week. Branch create is guarded by
// branchExists and the file write by the last_commit_id dispatch, so
// both are safe to repeat.
//
// This asymmetry existed because reads went through internal/gitlab's
// doJSON, which has retried 429/5xx all along, while every write came
// here and did not. One 502 during the unattended Sunday-night run lost
// the week; the discovery phase before it shrugged off the same error
// four times.
func (g *GitLabPublisher) api(ctx context.Context, urlStr, method string, body []byte, out any, retry bool) error {
	status, respBody, err := g.doRetry(ctx, urlStr, method, body, retry)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%s %s: %d %s", method, urlStr, status, textutil.Truncate(string(respBody), 300))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// doRetry performs one request, repeating it on 429 and 5xx when retry is
// set, and returns the final status and body. The body is bounded for
// the same reason internal/gitlab's reads are: this talks to a host, and
// an unbounded read is a remote-triggered OOM.
func (g *GitLabPublisher) doRetry(ctx context.Context, urlStr, method string, body []byte, retry bool) (int, []byte, error) {
	for attempt := 1; ; attempt++ {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", g.Token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := g.HTTP.Do(req)
		if err != nil {
			return 0, nil, err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()

		if !retry || attempt > maxRetries {
			return resp.StatusCode, respBody, nil
		}
		var wait time.Duration
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			wait = retryAfter(resp.Header.Get("Retry-After"))
		case resp.StatusCode >= 500:
			wait = backoff(attempt)
		default:
			return resp.StatusCode, respBody, nil
		}
		if err := sleepFn(ctx, wait); err != nil {
			return 0, nil, err
		}
	}
}
