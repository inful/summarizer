package publisher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitLab stands in for the three endpoints the publisher calls:
// POST /repository/branches, PUT /repository/files/:path,
// GET /repository/files/:path (for read-before-write), GET/POST /merge_requests.
// It records each request so the test can assert on the wire shape.
type fakeGitLab struct {
	branches      sync.Map // branch name → bool (existence)
	branchCreates atomic.Int64
	branchGets    atomic.Int64
	filePuts      atomic.Int64
	fileReads     atomic.Int64
	mergeRequests []mergeRequest
	mrCreateCount atomic.Int64

	// files pre-populates "branch -> path -> content" so read-before-write
	// tests can put a file on the branch before the publisher's
	// readExistingUID call. Path key is the full repo-relative path.
	files sync.Map // path -> content

	// lastPutContent is captured during the most recent PUT for tests
	// that need to assert on the rendered frontmatter (UID, fingerprint,
	// public flag). Wrapped by atomic.Value so tests can read safely
	// after Publish returns.
	lastPutContent atomic.Value // string

	// Hooks let the test force-fail specific paths.
	branchErr error
	fileErr   error
}

func (f *fakeGitLab) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if f.branchErr != nil {
			http.Error(w, f.branchErr.Error(), http.StatusInternalServerError)
			return
		}
		var b branchReq
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &b)
		// Real GitLab 400s on a duplicate branch name.
		if _, ok := f.branches.Load(b.Branch); ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":{"branch":["has already been taken"]}}`))
			return
		}
		f.branches.Store(b.Branch, true)
		f.branchCreates.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/branches/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		name, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(),
			"/api/v4/projects/group%2Freports/repository/branches/"))
		if err != nil {
			http.Error(w, "bad encoding", http.StatusBadRequest)
			return
		}
		if _, ok := f.branches.Load(name); !ok {
			http.Error(w, "404 Branch Not Found", http.StatusNotFound)
			return
		}
		f.branchGets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"x"}`))
	})

	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// readExistingUID: a 200 with the stored content (for an
			// existing-uid test); a 404 (for the fresh-file test).
			// r.URL.Path is the URL-decoded form, so the trim prefix
			// and the published path are both in unencoded form.
			const prefix = "/api/v4/projects/group/reports/repository/files/"
			path := strings.TrimPrefix(r.URL.Path, prefix)
			f.fileReads.Add(1)
			content, ok := f.files.Load(path)
			if !ok {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"encoding":"text","content":%q}`, content)
			return
		}
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if f.fileErr != nil {
			http.Error(w, f.fileErr.Error(), http.StatusInternalServerError)
			return
		}
		var fr fileReq
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &fr)
		// Verify content is actually base64 and decodes back to a non-empty body.
		decoded, err := base64.StdEncoding.DecodeString(fr.Content)
		if err != nil || len(decoded) == 0 {
			http.Error(w, "bad content", http.StatusBadRequest)
			return
		}
		if fr.Branch == "" || fr.CommitMessage == "" || fr.Encoding != "base64" {
			http.Error(w, "missing fields", http.StatusBadRequest)
			return
		}
		// Capture the decoded PUT body so UID-preservation tests can
		// assert on what's actually rendered.
		f.lastPutContent.Store(string(decoded))
		f.filePuts.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Filter by ?state= query param so the publisher's two-list
			// strategy works as expected on the wire.
			q := r.URL.Query()
			want := q.Get("state")
			filtered := make([]mergeRequest, 0)
			for _, mr := range f.mergeRequests {
				if want == "" || mr.State == want {
					filtered = append(filtered, mr)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(filtered)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var mr mrReq
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &mr)
		if mr.SourceBranch == "" || mr.TargetBranch == "" || mr.Title == "" {
			http.Error(w, "missing mr fields", http.StatusBadRequest)
			return
		}
		f.mrCreateCount.Add(1)
		f.mergeRequests = append(f.mergeRequests, mergeRequest{
			IID:          42,
			WebURL:       "https://gitlab.example.com/group/reports/-/merge_requests/42",
			State:        "opened",
			SourceBranch: mr.SourceBranch,
			TargetBranch: mr.TargetBranch,
			Title:        mr.Title,
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.mergeRequests[len(f.mergeRequests)-1])
	})
	return mux
}

// TestGitLabPublisher_PutSendsLastCommitIDForExistingFiles: GitLab
// 400s on PUT /repository/files with the misleading message "A
// file with this name doesn't exist" when the file is already on
// the branch and the body doesn't include the file's current
// `last_commit_id`. The publisher must round-trip the SHA from the
// GET (read-before-write) into the PUT body, or every re-run in
// monday-bucket mode 400s on the second file.
//
// Like the ref-field test, this inspects the raw PUT body rather
// than a struct decode so the test cannot pass by accident.
func TestGitLabPublisher_PutSendsLastCommitIDForExistingFiles(t *testing.T) {
	const lastCommitID = "abc1234567890abcdef1234567890abcdef12345"
	putBodies := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches") && !strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/branches/"):
			// Branch exists.
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/repository/files/"):
			if r.Method == http.MethodGet {
				// read-before-write: file exists, return its content
				// and last_commit_id. The body has the docbuilder
				// frontmatter with a uid; the publisher preserves it.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"encoding": "text",
					"content": "---\nuid: existing-uid\n---\n\nold body\n",
					"last_commit_id": "` + lastCommitID + `"
				}`))
				return
			}
			// PUT: capture the body so the test can assert on it.
			body, _ := io.ReadAll(r.Body)
			putBodies = append(putBodies, string(body))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"iid":1,"web_url":"http://x","source_branch":"b","target_branch":"main","title":"t","state":"opened"}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("new body"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(putBodies) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(putBodies))
	}
	wantSub := `"last_commit_id":"` + lastCommitID + `"`
	if !strings.Contains(putBodies[0], wantSub) {
		t.Errorf("PUT body must include last_commit_id %q; got %s", wantSub, putBodies[0])
	}
}

// TestGitLabPublisher_PutOmitsLastCommitIDForNewFiles: when the file
// doesn't exist on the branch (first publish of a period), the
// publisher must NOT include last_commit_id. Sending it for a
// non-existent file is the inverse of the previous bug — GitLab
// 400s on the wrong-shape body. omitempty does this correctly only
// if the field is left empty.
func TestGitLabPublisher_PutOmitsLastCommitIDForNewFiles(t *testing.T) {
	putBodies := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches") && !strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/repository/files/"):
			if r.Method == http.MethodGet {
				// 404: file doesn't exist yet. This is the "create"
				// path; the publisher must NOT include last_commit_id.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, _ := io.ReadAll(r.Body)
			putBodies = append(putBodies, string(body))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"iid":1,"web_url":"http://x","source_branch":"b","target_branch":"main","title":"t","state":"opened"}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("new body"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(putBodies) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(putBodies))
	}
	if strings.Contains(putBodies[0], "last_commit_id") {
		t.Errorf("PUT body must NOT include last_commit_id for a new file; got %s", putBodies[0])
	}
}

// TestGitLabPublisher_CreateOrUpdateDispatch: the publisher must use
// POST for the first publish (file doesn't exist on the branch) and
// PUT for subsequent re-runs (file exists, last_commit_id is round-
// tripped). The read signal is `last_commit_id` from the GET — empty
// means POST, non-empty means PUT. This is the bug that surfaced as
// `400 {"message":"A file with this name doesn't exist"}` on the
// first publish of every run against GitLab versions that do not
// auto-create on PUT.
func TestGitLabPublisher_CreateOrUpdateDispatch(t *testing.T) {
	// records every request the publisher makes, with method and path,
	// so the test can assert POST-then-PUT-then-... order.
	type req struct {
		method string
		path   string
	}
	var requests []req
	var mu sync.Mutex

	const lastCommitID = "frozen-sha-from-monday"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, req{method: r.Method, path: r.URL.Path})
		mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/branches") && !strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/repository/files/"):
			if r.Method == http.MethodGet {
				// The first publish: file doesn't exist (404).
				// The second publish: file exists with last_commit_id.
				if r.URL.Query().Get("ref") == "" {
					http.Error(w, "missing ref", http.StatusBadRequest)
					return
				}
				if !mu.TryLock() {
					// Two readers can race; serialise them.
					mu.Lock()
				}
				count := len(requests)
				mu.Unlock()
				if count <= 2 {
					// First read (branch create probe) just happened.
					// The 404 fires for this read.
					w.WriteHeader(http.StatusNotFound)
					return
				}
				// The second read: file exists, return content + SHA.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"encoding": "text",
					"content": "---\nuid: existing-uid\n---\n\nold body\n",
					"last_commit_id": "` + lastCommitID + `"
				}`))
				return
			}
			// PUT or POST: both 200 OK on success.
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"iid":1,"web_url":"http://x","source_branch":"b","target_branch":"main","title":"t","state":"opened"}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	// First publish: file does not exist. The publisher should
	// POST, never PUT, because the read returned 404 → empty
	// last_commit_id.
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("first body"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Second publish: file exists with the SHA from the read.
	// The publisher should PUT, never POST, because the read
	// returned 200 with last_commit_id non-empty.
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("second body"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	// Inspect the order of file operations: GET (read), POST or PUT.
	var files []string
	mu.Lock()
	for _, r := range requests {
		if strings.Contains(r.path, "/repository/files/") {
			files = append(files, r.method)
		}
	}
	mu.Unlock()

	// Each Publish triggers: GET (read), METHOD (create or update).
	// First publish: GET + POST.
	// Second publish: GET + PUT.
	if len(files) != 4 {
		t.Fatalf("want 4 file requests (2 GETs, 1 POST, 1 PUT); got %d: %v", len(files), files)
	}
	if files[0] != "GET" || files[1] != "POST" {
		t.Errorf("first publish should be GET then POST; got %v", files[:2])
	}
	if files[2] != "GET" || files[3] != "PUT" {
		t.Errorf("second publish should be GET then PUT; got %v", files[2:])
	}
}

// TestGitLabPublisher_EnsureBranchUsesRefField: GitLab's modern
// POST /repository/branches expects the start-point as `ref` (the
// legacy name `start_point` was renamed in GitLab 12.2 and modern
// instances 400 with "ref is missing, ref is empty" if the body
// uses the old name). The publisher must send the right field or
// the very first run against a real instance fails.
//
// This test reads the raw request body — not a struct decode —
// so the test fails the moment the JSON tag is wrong, even if
// the test's own branchReq stays in sync by accident.
func TestGitLabPublisher_EnsureBranchUsesRefField(t *testing.T) {
	var captured atomic.Value // raw request body
	captured.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/branches") && !strings.Contains(r.URL.Path, "/branches/") {
			body, _ := io.ReadAll(r.Body)
			captured.Store(string(body))
			w.WriteHeader(http.StatusCreated)
			return
		}
		if strings.Contains(r.URL.Path, "/branches/") {
			// ensureBranch's existence probe. The path contains
			// "/branches/<name>" for a GET, distinguishing it
			// from the POST to /branches (no trailing slash).
			// 404 = branch missing, so the publisher will POST to
			// create it.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/merge_requests") {
			if r.Method == http.MethodGet {
				// findOrCreateMR: GET returns the list (empty = no open MR).
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			} else {
				// POST: create a new MR, return the object.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"iid":1,"web_url":"http://x","source_branch":"summarizer/2026-10-19","target_branch":"main","title":"t","state":"opened"}`))
			}
			return
		}
		if strings.Contains(r.URL.Path, "/repository/files/") {
			switch r.Method {
			case http.MethodGet:
				// readExistingUID: 404 = no existing file, proceed.
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPut:
				// file create/update: 200 = committed.
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusOK)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	body := captured.Load().(string)
	if !strings.Contains(body, `"ref":"main"`) {
		t.Errorf("request body must use GitLab's modern field name \"ref\"; got %s", body)
	}
	if strings.Contains(body, "start_point") {
		t.Errorf("request body must NOT use the legacy \"start_point\" field; got %s", body)
	}
	if !strings.Contains(body, `"branch":"summarizer/2026-10-19"`) {
		t.Errorf("request body must name the new branch; got %s", body)
	}
}

func TestGitLabPublisher_PublishFlow(t *testing.T) {
	f := &fakeGitLab{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(
		srv.URL,         // host (no trailing path; client adds /api/v4)
		"glpat-test",    // token
		"group/reports", // project
		"reports",       // prefix
		"main",          // default branch
		"summarizer",    // source branch prefix
	)

	res, err := p.Publish(context.Background(), Request{
		Name:   "group-proj--7d--customer.md",
		Body:   []byte("# Customer weekly update\n\nShipped login.\n"),
		Force:  false,
		Kind:   KindPerProject,
		Period: "2026-10-19",
		Title:  "customer",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := f.branchCreates.Load(); got != 1 {
		t.Errorf("expected 1 branch creation, got %d", got)
	}
	if got := f.filePuts.Load(); got != 1 {
		t.Errorf("expected 1 file put, got %d", got)
	}
	if got := f.mrCreateCount.Load(); got != 1 {
		t.Errorf("expected 1 MR creation, got %d", got)
	}

	if res.Branch != "summarizer/2026-10-19" {
		t.Errorf("branch = %q, want summarizer/2026-10-19", res.Branch)
	}
	if !strings.HasPrefix(res.MRURL, "https://") || !strings.Contains(res.MRURL, "/merge_requests/42") {
		t.Errorf("MR URL unexpected: %q", res.MRURL)
	}
	if res.Ref == "" || !strings.Contains(res.Ref, "summarizer/2026-10-19") {
		t.Errorf("Ref should be a web URL pointing at the file: %q", res.Ref)
	}
	// Path is now <prefix>/<period>/<name> — confirm the period subdir
	// shows up in the Ref (the web URL encodes the path).
	if !strings.Contains(res.Ref, "reports/2026-10-19/") {
		t.Errorf("Ref should include prefix/period subdirectory: %q", res.Ref)
	}
}

func TestGitLabPublisher_IdempotentSamePeriod(t *testing.T) {
	// Pre-seed the branch as already-existing AND an open MR for the same
	// source branch. A re-run on the same period must NOT create a new
	// MR or branch — it just pushes additional commits.
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 7, WebURL: "https://gitlab.example.com/group/reports/-/merge_requests/7",
				State: "opened", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main",
				Title: "old but open"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	f.branches.Store("summarizer/2026-10-19", true)

	res, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.MRURL != "https://gitlab.example.com/group/reports/-/merge_requests/7" {
		t.Errorf("expected existing MR URL, got %q", res.MRURL)
	}
	if got := f.branchCreates.Load(); got != 0 {
		t.Errorf("expected no new branch creation, got %d", got)
	}
	if got := f.mrCreateCount.Load(); got != 0 {
		t.Errorf("expected no new MR creation, got %d", got)
	}
	if got := f.filePuts.Load(); got != 1 {
		t.Errorf("expected 1 file put, got %d", got)
	}
	_ = time.Now() // silence unused import linter
}

// TestGitLabPublisher_MultipleFilesOneBranch: one run publishes
// several reports into the same period. They share one branch and one
// MR. Real GitLab 400s ("has already been taken") when POST
// /branches is called for a branch that exists — the fake used to
// paper over that with a 200, which let a second-file publish fail
// against live GitLab. The branch must be created exactly once.
func TestGitLabPublisher_MultipleFilesOneBranch(t *testing.T) {
	f := &fakeGitLab{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	for _, name := range []string{"a--7d--customer.md", "a--7d--team.md", "combined--7d--customer.md"} {
		if _, err := p.Publish(context.Background(), Request{
			Name: name, Body: []byte("body " + name), Period: "2026-10-19", Kind: KindPerProject,
		}); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}
	if got := f.branchCreates.Load(); got != 1 {
		t.Errorf("branch created %d times, want exactly 1", got)
	}
	if got := f.mrCreateCount.Load(); got != 1 {
		t.Errorf("MR created %d times, want exactly 1", got)
	}
	if got := f.filePuts.Load(); got != 3 {
		t.Errorf("file puts = %d, want 3", got)
	}
}

func TestGitLabPublisher_RejectsMergedMR(t *testing.T) {
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 5, WebURL: "x", State: "merged", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main", Title: "old"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	_, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	})
	if err == nil {
		t.Fatal("expected error when prior MR is merged")
	}
	if !strings.Contains(err.Error(), "merged") {
		t.Errorf("error should mention merged state: %v", err)
	}
}

// TestGitLabPublisher_PreservesExistingUID: when the file is already
// on the branch from an earlier run with a docbuilder-format uid, the
// publisher reuses it instead of minting a new UUID v7. This is the
// docbuilder invariant: the same file in two projects must link
// identically, so the UID must survive re-runs across the day.
func TestGitLabPublisher_PreservesExistingUID(t *testing.T) {
	// Canonical UUID v7 shape: the third group's first hex digit is
	// the version (`7`), the fourth group's first hex digit is the
	// variant (8..b). mnidoid mints these and mdid's test code pins them.
	const existingUID = "019200a0-72cd-7abc-8def-0123456789ab"

	f := &fakeGitLab{}
	// Pre-populate the file on the branch with the existing UID baked
	// into its frontmatter — exactly what a previous Publish would have
	// landed. Period "2026-10-19" + Name "x.md" → targetPath:
	// "reports/2026-10-19/x.md". File content shape is the same
	// docbuilder frontmatter Render() would emit.
	const path = "reports/2026-10-19/x.md"
	f.files.Store(path, fmt.Sprintf(`---
uid: %s
title: "x"
date: 2026-10-19T09:00:00Z
lastmod: 2026-10-19T09:00:00Z
fingerprint: deadbeef00000000000000000000000000000000000000000000000000000000
categories:
  - engineering-update
tags:
  - summarizer
  - customer
  - 2026-10-19
  - per-project
  - group/proj
aliases:
  - /_uid/%s/
---

old body`, existingUID, existingUID))

	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	_, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("# new body"), Period: "2026-10-19",
		Kind: KindPerProject, Title: "customer", Project: "group/proj", Public: true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := f.fileReads.Load(); got != 1 {
		t.Errorf("expected 1 read-before-write, got %d", got)
	}
	if got := f.filePuts.Load(); got != 1 {
		t.Errorf("expected 1 file put, got %d", got)
	}
	got := f.lastPutContent.Load().(string)
	if !strings.Contains(got, "uid: "+existingUID) {
		t.Errorf("re-run should preserve existing uid %q, got:\n%s", existingUID, got)
	}
	// Body should be the new one, not the old.
	if !strings.Contains(got, "# new body") {
		t.Errorf("re-run should write new body, got:\n%s", got)
	}
	if strings.Contains(got, "old body") {
		t.Errorf("re-run should overwrite old body, got:\n%s", got)
	}
}

// TestGitLabPublisher_FreshFileMintsUUID: when no file exists yet on
// the branch (first publish for the period), the publisher mints a new
// UUID v7 via mdid — distinct on every call. Verifies the read call
// returns 404 (not an error) and the mint path fires.
func TestGitLabPublisher_FreshFileMintsUUID(t *testing.T) {
	uuidV7Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	f := &fakeGitLab{} // no .files populated → 404 on read
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	_, err := p.Publish(context.Background(), Request{
		Name: "fresh.md", Body: []byte("hello"), Period: "2026-10-19",
		Kind: KindPerProject, Title: "team", Project: "group/proj",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := f.fileReads.Load(); got != 1 {
		t.Errorf("expected 1 read-before-write attempt, got %d", got)
	}
	if got := f.filePuts.Load(); got != 1 {
		t.Errorf("expected 1 file put, got %d", got)
	}
	got := f.lastPutContent.Load().(string)
	if !strings.Contains(got, "uid: ") {
		t.Fatalf("rendered frontmatter missing uid line:\n%s", got)
	}
	// Extract the uid value and check it's a v7 UUID.
	uidLine := ""
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "uid: ") {
			uidLine = strings.TrimPrefix(line, "uid: ")
			break
		}
	}
	if !uuidV7Re.MatchString(uidLine) {
		t.Errorf("fresh-file mint should produce a UUID v7, got %q", uidLine)
	}
}
