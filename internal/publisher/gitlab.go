package publisher

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inful/mdfm"
	"github.com/inful/summarizer/internal/textutil"
)

// GitLabPublisher commits each report to a per-period source branch on
// the target project and opens/updates a merge request so operators
// (or project owners) can review and amend before publishing.
//
// Typical flow per period (e.g. "2026-10-19"):
//
//  1. POST  /projects/:id/repository/branches  (summarizer/2026-10-19, from main) — once per period
//  2. PUT   /projects/:id/repository/files/:path?branch=summarizer/2026-10-19 — once per file
//  3. GET   /projects/:id/merge_requests?source_branch=summarizer/2026-10-19
//     - none: POST .../merge_requests
//     - open:  leave as-is; the new commit stacks on the existing MR
//     - merged / closed: skip with Result.Skipped + clear stderr message
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
//	reports/2026-10-13/  group-proj--7d--customer.md
//	                     group-proj--7d--team.md
//	                     combined--7d--customer.md
//	                     combined--7d--team.md
//	reports/2026-10-20/  ...
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
func (g *GitLabPublisher) Publish(req Request) (Result, error) {
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
	existingUID, lastCommitID, err := g.readExistingUID(path, branch)
	if err != nil {
		return Result{}, fmt.Errorf("read existing %s@%s: %w", path, branch, err)
	}

	// Wrap the body with docbuilder-format frontmatter. The fingerprint
	// in the frontmatter is the SHA-256 of the raw body via mdfp, so even
	// if the title/date change between runs, a content-equivalent re-run
	// is detectable by docbuilder. Public is set per Request — customer
	// reports get public: true so docbuilder exposes them externally;
	// team reports omit the key (internal-only).
	fm := NewFrontmatter(req.Project, req.Title, req.Period, req.Kind, time.Now().UTC(), req.Body, existingUID)
	fm.Public = req.Public
	finalBody := fm.Render(req.Body)
	encoded := base64.StdEncoding.EncodeToString(finalBody)

	// 1. Ensure the per-period source branch exists.
	if err := g.ensureBranch(branch); err != nil {
		return Result{}, fmt.Errorf("ensure branch %s: %w", branch, err)
	}

	// 2. PUT the file onto the branch.
	putURL := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s",
		g.Host, url.PathEscape(g.Project), pathEscape(path))
	putBody, _ := json.Marshal(fileReq{
		Branch:        branch,
		CommitMessage: commitMessage(req),
		Content:       encoded,
		Encoding:      "base64",
		LastCommitID:  lastCommitID,
	})
	if err := g.api(putURL, "PUT", putBody, nil); err != nil {
		return Result{}, fmt.Errorf("put file %s@%s: %w", path, branch, err)
	}

	// 3. Find or create the MR for this period.
	mrURL, err := g.findOrCreateMR(branch, req)
	if err != nil {
		// File landed; an MR-side failure should still report the branch ref.
		return Result{Ref: fmt.Sprintf("%s/-/blob/%s/%s", g.Host, branch, path), Branch: branch}, err
	}
	return Result{
		Ref:    fmt.Sprintf("%s/-/blob/%s/%s", g.Host, branch, path),
		Branch: branch,
		MRURL:  mrURL,
	}, nil
}

// ensureBranch creates the per-period source branch if it's missing.
// GET-first: GitLab 400s with "has already been taken" on a duplicate
// branch name, and one run publishes many files into one period — so
// the naive POST-every-time version died on the second report.
func (g *GitLabPublisher) ensureBranch(branch string) error {
	if g.branchExists(branch) {
		return nil
	}
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches",
		g.Host, url.PathEscape(g.Project))
	body, _ := json.Marshal(branchReq{
		Branch: branch,
		Ref:    g.DefaultBranch,
	})
	return g.api(urlStr, "POST", body, nil)
}

// branchExists treats only 200 as "yes"; every other status (404, 403,
// transport) falls through to the create attempt, which surfaces the
// real error rather than swallowing it here.
func (g *GitLabPublisher) branchExists(branch string) bool {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches/%s",
		g.Host, url.PathEscape(g.Project), pathEscape(branch))
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
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
func (g *GitLabPublisher) readExistingUID(path, branch string) (uid, lastCommitID string, err error) {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s?ref=%s",
		g.Host, url.PathEscape(g.Project), pathEscape(path), url.QueryEscape(branch))

	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
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
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("%s %s: %d %s",
			http.MethodGet, urlStr, resp.StatusCode, truncate(string(body), 300))
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
		// tool other than summarizer — all rare and recoverable. Treat
		// as "no usable uid" so the next publish mints fresh; the
		// last_commit_id still flows so the PUT authorises the update.
		return "", fr.LastCommitID, nil
	}
	return parsedUID, fr.LastCommitID, nil
}

// mergeRequests lists MRs; we use a wide state filter so we can detect
// both "none" and "already merged/closed" cleanly.
func (g *GitLabPublisher) mergeRequests(sourceBranch, state string) ([]mergeRequest, error) {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?source_branch=%s&state=%s",
		g.Host, url.PathEscape(g.Project), url.QueryEscape(sourceBranch), url.QueryEscape(state))
	var out []mergeRequest
	if err := g.api(urlStr, "GET", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// findOrCreateMR opens the MR if one isn't open for this source branch.
// Returns the MR web URL. If an MR exists in merged / closed state, we
// decline to re-open it and return a Skipped result via the returned
// error; the caller decides what to do.
func (g *GitLabPublisher) findOrCreateMR(branch string, req Request) (string, error) {
	mrs, err := g.mergeRequests(branch, "opened")
	if err != nil {
		return "", err
	}
	if len(mrs) > 0 {
		return mrs[0].WebURL, nil
	}

	// No open MR. Check merged/closed before creating a fresh one.
	nonOpen, err := g.mergeRequests(branch, "merged")
	if err != nil {
		return "", err
	}
	if len(nonOpen) > 0 {
		return "", fmt.Errorf("existing MR for period %s is %s; refusing to reopen — merge the existing MR or use a different period", req.Period, nonOpen[0].State)
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
				"- **Period:** %s\n"+
				"- **Project(s):** %s\n"+
				"- **Audience:** %s\n\n"+
				"Files in this branch have docbuilder-format frontmatter.\n\n"+
				"Merge to publish.",
			req.Period, req.Name, req.Kind),
	})
	var mr mergeRequest
	if err := g.api(createURL, "POST", body, &mr); err != nil {
		return "", err
	}
	return mr.WebURL, nil
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
func (g *GitLabPublisher) api(urlStr, method string, body []byte, out any) error {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %d %s", method, urlStr, resp.StatusCode, truncate(string(respBody), 300))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func truncate(s string, n int) string { return textutil.Truncate(s, n) }
