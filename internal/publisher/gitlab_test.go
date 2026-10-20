package publisher

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitLab stands in for the three endpoints the publisher calls:
// POST /repository/branches, PUT /repository/files/:path, GET/POST /merge_requests.
// It records each request so the test can assert on the wire shape.
type fakeGitLab struct {
	branches        sync.Map // branch name → bool (existence)
	branchCreates   atomic.Int64
	filePuts        atomic.Int64
	mergeRequests   []mergeRequest
	mrCreateCount   atomic.Int64

	// Hooks let the test force-fail specific paths.
	branchErr  error
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
		json.Unmarshal(body, &b)
		// Idempotent: if the branch exists, return ok.
		if _, ok := f.branches.Load(b.Branch); ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		f.branches.Store(b.Branch, true)
		f.branchCreates.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if f.fileErr != nil {
			http.Error(w, f.fileErr.Error(), http.StatusInternalServerError)
			return
		}
		var fr fileReq
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &fr)
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
			json.NewEncoder(w).Encode(filtered)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var mr mrReq
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &mr)
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
		json.NewEncoder(w).Encode(f.mergeRequests[len(f.mergeRequests)-1])
	})
	return mux
}

func TestGitLabPublisher_PublishFlow(t *testing.T) {
	f := &fakeGitLab{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(
		srv.URL,        // host (no trailing path; client adds /api/v4)
		"glpat-test",   // token
		"group/reports", // project
		"reports",      // prefix
		"main",         // default branch
		"summarizer",   // source branch prefix
	)

	res, err := p.Publish(Request{
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

	res, err := p.Publish(Request{
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

func TestGitLabPublisher_RejectsMergedMR(t *testing.T) {
	f := &fakeGitLab{
		mergeRequests: []mergeRequest{
			{IID: 5, WebURL: "x", State: "merged", SourceBranch: "summarizer/2026-10-19", TargetBranch: "main", Title: "old"},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := NewGitLab(srv.URL, "glpat-test", "group/reports", "reports", "main", "summarizer")

	_, err := p.Publish(Request{
		Name: "x.md", Body: []byte("x"), Period: "2026-10-19", Kind: KindPerProject,
	})
	if err == nil {
		t.Fatal("expected error when prior MR is merged")
	}
	if !strings.Contains(err.Error(), "merged") {
		t.Errorf("error should mention merged state: %v", err)
	}
}
