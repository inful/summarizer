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
	"os"
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
	// Flaky counters: the first N writes / MR creates answer 503, so a
	// test can tell a retried write from a duplicated one. Guarded by
	// mu where a handler mutates them.
	flakyWrites           int
	flakyMRCreates        int
	filePutAttempts       atomic.Int64
	mrCreateAttempts      atomic.Int64
	branchExistsCount     atomic.Int64
	mergeRequestListCount atomic.Int64
	// mrCreateFails makes the MR create always error, so a test can
	// tell "memoised a failure" from "memoised a success".
	mrCreateFails bool
	fileReads     atomic.Int64
	mrCreateCount atomic.Int64

	// Guarded by mu, not atomics: these are slices and strings that
	// the handler writes on every request. TestGitLabPublisher_ConcurrentPublishAcrossBranchesIsRaceFree
	// drives six goroutines through this handler at once, and unsynchronised
	// writes here made the race detector fail the fixture rather than the
	// publisher — a test that can only fail for the wrong reason is
	// worse than no test. The counters above stayed atomic because they
	// are read individually from the test goroutine.
	mu            sync.Mutex
	lastMRDesc    string
	mergeRequests []mergeRequest

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
		f.branchExistsCount.Add(1)
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
		f.filePutAttempts.Add(1)
		if f.flakyWrites > 0 {
			f.mu.Lock()
			f.flakyWrites--
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.filePuts.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mergeRequestListCount.Add(1)
			// Filter by ?state= query param so the publisher's two-list
			// strategy works as expected on the wire.
			// Filter on BOTH source_branch and state, as GitLab does.
			// Filtering on state alone made this fake report every
			// branch's open MR to every caller, so a goroutine asking
			// about its own branch would find another branch's MR and
			// skip creating its own —
			// TestGitLabPublisher_ConcurrentPublishAcrossBranchesIsRaceFree
			// then failed intermittently with "5, want 6". The publisher
			// has always sent source_branch; the fixture just ignored it.
			q := r.URL.Query()
			wantState, wantBranch := q.Get("state"), q.Get("source_branch")
			filtered := make([]mergeRequest, 0)
			f.mu.Lock()
			for _, mr := range f.mergeRequests {
				if wantState != "" && mr.State != wantState {
					continue
				}
				if wantBranch != "" && mr.SourceBranch != wantBranch {
					continue
				}
				filtered = append(filtered, mr)
			}
			f.mu.Unlock()
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
		f.mu.Lock()
		f.lastMRDesc = mr.Description
		flaky := f.flakyMRCreates > 0
		if flaky {
			f.flakyMRCreates--
		}
		f.mu.Unlock()
		f.mrCreateAttempts.Add(1)
		if flaky {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.mrCreateFails {
			http.Error(w, "create refused", http.StatusUnprocessableEntity)
			return
		}
		f.mrCreateCount.Add(1)
		created := mergeRequest{
			IID:          42,
			WebURL:       "https://gitlab.example.com/group/reports/-/merge_requests/42",
			State:        "opened",
			SourceBranch: mr.SourceBranch,
			TargetBranch: mr.TargetBranch,
			Title:        mr.Title,
		}
		f.mu.Lock()
		f.mergeRequests = append(f.mergeRequests, created)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(created)
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
		Kind:   KindPerProject,
		Period: "2026-10-19",
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

// TestGitLabPublisher_MRBodyNamesVisibilityNotKind: the row was headed
// "- **Audience:**" and filled with req.Kind, so every merge request
// read "Audience: rollup" — a file category under a label that means
// who the document is for. The MR is what a human reads before
// merging, so the wrong label is worse than no label.
func TestGitLabPublisher_MRBodyNamesVisibilityNotKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     Request
		want    string
		notWant string
	}{
		{"customer file", Request{Name: "g-p--7d--customer.md", Kind: KindPerProject,
			Public: true}, "customer-facing", "per-project"},
		{"team rollup", Request{Name: "combined--7d--team.md", Kind: KindRollup,
			Public: false}, "internal", "rollup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitLab{}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
			tc.req.Body, tc.req.Period = []byte("x"), "2026-10-19"
			if _, err := p.Publish(context.Background(), tc.req); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if !strings.Contains(f.lastMRDesc, tc.want) {
				t.Errorf("MR body should state %q, got:\n%s", tc.want, f.lastMRDesc)
			}
			if strings.Contains(f.lastMRDesc, "**Audience:**") {
				t.Errorf("the Audience row should be gone; got:\n%s", f.lastMRDesc)
			}
			if strings.Contains(f.lastMRDesc, "**Visibility:** "+tc.notWant) {
				t.Errorf("visibility %q should not be the file kind %q; got:\n%s",
					tc.want, tc.notWant, f.lastMRDesc)
			}
		})
	}
}

func TestGitLabPublisher_IdempotentSamePeriod(t *testing.T) {
	// Pre-seed the branch as already-existing AND an open MR for the same
	// source branch. A re-run on the same period must NOT create a new
	// MR or branch — it just pushes additional commits.
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 7, WebURL: "https://gitlab.example.com/group/reports/-/merge_requests/7",
				State: "opened", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main"},
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

// TestGitLabPublisher_MergedPeriodIsALoggedNoOp: re-running a week
// whose MR was already merged is the ORDINARY response to a report that
// said the wrong thing. It used to return an error, which the pipeline
// surfaced as two failed projects, skipped both cross-project rollups
// and exited non-zero — even though the file had been written
// successfully, onto a branch that will never merge again.
func TestGitLabPublisher_MergedPeriodIsALoggedNoOp(t *testing.T) {
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 5, WebURL: "x", State: "merged", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	res, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	})
	if err != nil {
		t.Fatalf("a re-run of an already-published week must not fail: %v", err)
	}
	if !res.Closed {
		t.Error("Result.Closed should be set so the caller can report it")
	}
	if res.Ref == "" {
		t.Error("the branch ref should still be returned; the file did land")
	}
}

// TestGitLabPublisher_ClosedMRCountsAsPublished: closing an MR without
// merging is the ordinary way to abandon a week. findOrCreateMR
// queried only state=merged, so this state was invisible to it and the
// next run opened a SECOND MR for the same branch.
func TestGitLabPublisher_ClosedMRCountsAsPublished(t *testing.T) {
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 6, WebURL: "x", State: "closed", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	res, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	})
	if err != nil {
		t.Fatalf("a closed period should be a no-op, not an error: %v", err)
	}
	if !res.Closed {
		t.Error("a closed MR should mark the period as already published")
	}
	if got := f.mrCreateCount.Load(); got != 0 {
		t.Errorf("created %d MR(s) for a week that already had a closed one; "+
			"that is how a second MR for the same branch appeared", got)
	}
}

// TestGitLabPublisher_MissingUIDIsReportedNotSilent: the uid is
// docbuilder's cross-project link identity — the entire reason for
// read-before-write. A file with no uid (or no frontmatter at all) is
// re-minted, which is the recoverable half; the broken links are not,
// so stderr has to say which file lost one.
func TestGitLabPublisher_MissingUIDIsReportedNotSilent(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no frontmatter", "# just a report\n"},
		{"frontmatter without uid", "---\ntitle: \"x\"\n---\n\nbody\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitLab{}
			const path = "reports/2026-10-19/x.md"
			f.files.Store(path, tc.body)
			srv := httptest.NewServer(f.handler())
			defer srv.Close()

			out := captureStderr(t, func() {
				p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
				if _, err := p.Publish(context.Background(), Request{
					Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
				}); err != nil {
					t.Fatalf("Publish: %v", err)
				}
			})
			if !strings.Contains(out, "uid") || !strings.Contains(out, path) {
				t.Errorf("losing the uid must be reported, naming the file. stderr:\n%s", out)
			}
		})
	}
}

// captureStderr runs fn with stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = old
	return <-done
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
		// Not an H1: NewFrontmatter strips a leading "# ..." title so the
		// body does not duplicate the frontmatter one, and this test is
		// about which body landed, not about titles.
		Name: "x.md", Body: []byte("new body"), Period: "2026-10-19",
		Kind: KindPerProject, Project: "group/proj", Public: true,
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
	if !strings.Contains(got, "new body") {
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
		Kind: KindPerProject, Project: "group/proj",
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

// TestGitLabPublisher_RetriesIdempotentWrites: the reads go through
// internal/gitlab's doJSON, which has retried 429/5xx all along, while
// every write came through api() and did not. One 502 during the
// unattended Sunday-night run lost the week — after the discovery phase
// had shrugged off the same error four times.
func TestGitLabPublisher_RetriesIdempotentWrites(t *testing.T) {
	// No real backoff: the retry path is what is under test.
	restore := swapSleep(t)
	defer restore()
	f := &fakeGitLab{flakyWrites: 2}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	}); err != nil {
		t.Fatalf("a file write should survive two 503s: %v", err)
	}
	if got := f.filePuts.Load(); got != 1 {
		t.Errorf("file put %d times, want 1 logical write (%d attempts is fine)", got, got)
	}
}

// TestGitLabPublisher_DoesNotRetryTheMRCreate: GitLab does not
// de-duplicate merge requests, so a retried POST that actually
// succeeded the first time opens two MRs for one week.
// swapSleep makes backoff instantaneous for one test.
func swapSleep(t *testing.T) func() {
	t.Helper()
	old := sleepFn
	sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return func() { sleepFn = old }
}

func TestGitLabPublisher_DoesNotRetryTheMRCreate(t *testing.T) {
	f := &fakeGitLab{flakyMRCreates: 1}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")
	// The MR create fails; Publish must surface it rather than
	// retrying into a duplicate.
	if _, err := p.Publish(context.Background(), Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	}); err == nil {
		t.Fatal("expected the MR create error to surface")
	}
	if got := f.mrCreateAttempts.Load(); got != 1 {
		t.Errorf("attempted the MR create %d times, want exactly 1", got)
	}
}

// TestGitLabPublisher_MemoisesBranchScopedLookups: one run publishes
// every file onto one branch, and four of the six requests in Publish
// were branchExists plus three mergeRequests calls, all answering the
// same question for every file. On a 50-project run that is ~400
// requests re-asking what the first file already established.
func TestGitLabPublisher_MemoisesBranchScopedLookups(t *testing.T) {
	f := &fakeGitLab{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	const files = 5
	for i := range files {
		if _, err := p.Publish(context.Background(), Request{
			Name: fmt.Sprintf("f%d.md", i), Body: []byte("x"),
			Period: "2026-10-19", Kind: KindPerProject,
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	branchGets := f.branchExistsCount.Load()
	mrLists := f.mergeRequestListCount.Load()

	if branchGets != 1 {
		t.Errorf("branch-existence GET ran %d times for %d files on one branch, want 1", branchGets, files)
	}
	// One resolution: opened MR lookup, then merged + closed for the
	// miss, then the create. Everything after that is cached.
	if mrLists > 3 {
		t.Errorf("merge-request list ran %d times for %d files on one branch, want at most 3", mrLists, files)
	}
	if got := f.mrCreateCount.Load(); got != 1 {
		t.Errorf("created %d MRs for one branch, want exactly 1", got)
	}
}

// TestGitLabPublisher_MemoDoesNotCreateASecondMR: the cache must never
// record "there is no MR", or the second file would create one.
func TestGitLabPublisher_MemoDoesNotCreateASecondMR(t *testing.T) {
	f := &fakeGitLab{mrCreateFails: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	for i := range 2 {
		_, _ = p.Publish(context.Background(), Request{
			Name: fmt.Sprintf("f%d.md", i), Body: []byte("x"),
			Period: "2026-10-19", Kind: KindPerProject,
		})
	}
	// The first create failed, so the second file must try again rather
	// than trust a cached "none".
	if got := f.mrCreateAttempts.Load(); got != 2 {
		t.Errorf("attempted the MR create %d times, want 2 — a failed create must not be memoised", got)
	}
}

// TestGitLabPublisher_EveryFileOfARepublishedWeekIsMarkedClosed:
// the memo returned ("", nil) for a branch it knew was closed, so only
// the FIRST file of a re-published week produced errPeriodClosed and
// got Result.Closed. Every file after it was counted by printSummary
// as published — so a 50-project re-run reported "100 file(s) written
// (1 already published this week)" when nothing had reached main. That
// is the exact sentence the feature was added to fix.
func TestGitLabPublisher_EveryFileOfARepublishedWeekIsMarkedClosed(t *testing.T) {
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 5, WebURL: "x", State: "merged",
				SourceBranch: "summarizer/2026-10-19", TargetBranch: "main"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	const files = 5
	for i := range files {
		res, err := p.Publish(context.Background(), Request{
			Name: fmt.Sprintf("f%d.md", i), Body: []byte("x"),
			Period: "2026-10-19", Kind: KindPerProject,
		})
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if !res.Closed {
			t.Errorf("file %d: Closed is false — a re-published week must mark EVERY file, "+
				"not just the first", i)
		}
	}
}

// TestGitLabPublisher_MemoRemembersAPreExistingBranch: ensureBranch
// learned the branch existed from the GET and then discarded it, so the
// memo only paid off when this run created the branch — and not on a
// re-run, a restart mid-week, or the second `summarizer run` of an
// evening, which is when an operator iterates on a report.
func TestGitLabPublisher_MemoRemembersAPreExistingBranch(t *testing.T) {
	f := &fakeGitLab{}
	// The branch already exists before the run starts.
	f.branches.Store("summarizer/2026-10-19", struct{}{})
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	const files = 4
	for i := range files {
		if _, err := p.Publish(context.Background(), Request{
			Name: fmt.Sprintf("f%d.md", i), Body: []byte("x"),
			Period: "2026-10-19", Kind: KindPerProject,
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if got := f.branchExistsCount.Load(); got != 1 {
		t.Errorf("branch-existence GET ran %d times for %d files on an existing branch, want 1", got, files)
	}
}

// mergedWeekRepo models a reports repo whose last week was merged and
// whose source branch was then deleted — GitLab's "delete source
// branch on merge" project setting, or a human ticking that box in the
// merge dialog. The file and its uid survive on the default branch;
// nothing is left on summarizer/<period>.
type mergedWeekRepo struct {
	uidOnMain   string
	lastCommit  string
	branches    map[string]bool // which branches exist
	publishedID string          // uid observed in the last PUT
	reads       []string        // refs the file was read from
}

func (m *mergedWeekRepo) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/"+url.PathEscape("group/reports")+"/repository/branches", func(w http.ResponseWriter, r *http.Request) {
		// Creating a branch in GitLab forks it from the default
		// branch, so it immediately carries main's tree. The file
		// handler below relies on this.
		var req branchReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.branches[req.Branch] = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"` + req.Branch + `"}`))
	})
	mux.HandleFunc("/api/v4/projects/"+url.PathEscape("group/reports")+"/repository/branches/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/group%2Freports/repository/branches/")
		n, _ := url.PathUnescape(name)
		if m.branches[n] {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"` + n + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v4/projects/"+url.PathEscape("group/reports")+"/repository/files/", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			var req fileReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			raw, _ := base64.StdEncoding.DecodeString(req.Content)
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "uid: ") {
					m.publishedID = strings.TrimPrefix(line, "uid: ")
				}
			}
			if r.Method == http.MethodPut {
				_, _ = w.Write([]byte(`{"file_path":"x"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"file_path":"x"}`))
			return
		}
		m.reads = append(m.reads, ref)
		// The file is on main. A branch that exists carries main's
		// tree, so a read on any existing branch resolves it too; a
		// branch that does not exist 404s along with everything on it.
		if ref != "main" && !m.branches[ref] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := "---\nuid: " + m.uidOnMain + "\ntitle: t\n---\n\nlast week's report\n"
		b, _ := json.Marshal(body)
		_, _ = w.Write([]byte(`{"encoding":"text","content":` + string(b) +
			`,"last_commit_id":"` + m.lastCommit + `"}`))
	})
	mux.HandleFunc("/api/v4/projects/"+url.PathEscape("group/reports")+"/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"iid":1,"web_url":"https://gitlab.example.com/mr/1"}]`))
	})
	return mux
}

// TestPublish_KeepsTheUIDWhenTheSourceBranchWasDeleted: re-running a
// merged week is the ordinary response to a report that said the wrong
// thing. The branch read used to happen before ensureBranch, so a week
// whose branch was cleaned up looked like a first publish and got a
// fresh UUID — silently breaking every /_uid/ link to a document that
// was live on main.
func TestPublish_KeepsTheUIDWhenTheSourceBranchWasDeleted(t *testing.T) {
	const uidOnMain = "0192b6a0-72cd-7abc-8def-0123456789ab"
	repo := &mergedWeekRepo{
		uidOnMain:  uidOnMain,
		lastCommit: "abc1234",
		branches:   map[string]bool{}, // summarizer/2026-W43 is gone
	}
	srv := httptest.NewServer(repo.handler())
	defer srv.Close()

	g := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")
	if _, err := g.Publish(context.Background(), Request{
		Name: "g_p--wk--team.md", Body: []byte("this week's report"),
		Kind: KindPerProject, Period: "2026-W43", Audience: "team", Project: "g/p",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if repo.publishedID != uidOnMain {
		t.Errorf("published uid = %q, want the one already on main (%q) — the "+
			"document keeps its identity or every /_uid/ link to it breaks",
			repo.publishedID, uidOnMain)
	}
	if !repo.branches["summarizer/2026-W43"] {
		t.Error("the period branch should have been recreated from main")
	}
}

// TestGitLabPublisher_ConcurrentPublishAcrossBranchesIsRaceFree: the
// Publisher contract promises concurrency safety for DIFFERENT periods
// and explicitly does not promise it for one. This pins the half that
// holds, so "it is concurrency-safe" stays a claim with a test behind
// it rather than a comment.
//
// Meaningful under -race: g.cache, branchKnown, remember and recalled
// are the only shared state, and a lost update or a concurrent map
// write would fail here. The CI job runs `go test -race`.
//
// The same-period case is deliberately NOT exercised: it is the
// check-then-act in findOrCreateMR, which would open two merge requests,
// and that is documented rather than fixed.
func TestGitLabPublisher_ConcurrentPublishAcrossBranchesIsRaceFree(t *testing.T) {
	f := &fakeGitLab{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	g := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")

	const periods = 6
	var wg sync.WaitGroup
	errs := make([]error, periods)
	for i := range periods {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			period := fmt.Sprintf("2026-W%02d", 40+i)
			_, errs[i] = g.Publish(context.Background(), Request{
				Name:     fmt.Sprintf("p%d--wk--team.md", i),
				Body:     []byte(fmt.Sprintf("week %d body", i)),
				Kind:     KindPerProject,
				Period:   period,
				Audience: "team",
				Project:  "g/p",
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("period 2026-W%02d: %v", 40+i, err)
		}
	}
	if got := f.mrCreateAttempts.Load(); got != periods {
		t.Errorf("merge requests created = %d, want %d (one per branch, no duplicates)",
			got, periods)
	}
}
