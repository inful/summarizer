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
	Host       string // e.g. "https://gitlab.example.com"
	Token      string // PAT with api scope on the target project
	Project    string // path "group/subgroup/project"
	Prefix     string // path prefix for reports, e.g. "reports" ("" = repo root)
	DefaultBranch string // typically "main"; MR target
	SourceBranchPrefix string // e.g. "summarizer"; full branch is "<prefix>/<period>"
	HTTP *http.Client
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
	Branch string `json:"branch"`
	StartPoint string `json:"start_point,omitempty"`
}

type fileReq struct {
	Branch        string `json:"branch"`
	CommitMessage string `json:"commit_message"`
	Content       string `json:"content"`     // base64
	Encoding      string `json:"encoding"`    // "base64"
}

type mrReq struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	RemoveSourceBranch bool `json:"remove_source_branch,omitempty"`
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

	// Wrap the body with docbuilder-format frontmatter. The fingerprint
	// in the frontmatter is a SHA-256 of the raw body, so even if the
	// title/date change between runs, a content-equivalent re-run is
	// detectable by docbuilder. Public is set per Request — customer
	// reports get public: true so docbuilder exposes them externally;
	// team reports omit the key (internal-only).
	fm := NewFrontmatter(req.Project, req.Title, req.Period, req.Kind, time.Now().UTC(), req.Body)
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

func (g *GitLabPublisher) ensureBranch(branch string) error {
	urlStr := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches",
		g.Host, url.PathEscape(g.Project))
	body, _ := json.Marshal(branchReq{
		Branch:     branch,
		StartPoint: g.DefaultBranch,
	})
	return g.api(urlStr, "POST", body, nil)
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
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %d %s", method, urlStr, resp.StatusCode, truncate(string(respBody), 300))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
