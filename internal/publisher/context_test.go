package publisher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGitLabPublisher_ContextCancelsPublish: the whole point of
// threading the context through was that Ctrl-C could interrupt a
// publish. A cancelled context must surface as an error rather than
// completing the write.
func TestGitLabPublisher_ContextCancelsPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the request context is cancelled, mimicking a
		// GitLab that has stopped responding mid-publish.
		<-r.Context().Done()
	}))
	defer srv.Close()

	g := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := g.Publish(ctx, Request{
			Name:   "a--7d--team.md",
			Period: "2026-W42",
			Kind:   KindPerProject,
			Title:  "team",
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled context must not report success")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
			t.Errorf("error should be the context failure, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publish ignored the cancelled context and blocked")
	}
}

// TestGitLabPublisher_ReadHelpersHonourContext: the monthly read path
// has the same obligation as the write path.
func TestGitLabPublisher_ReadHelpersHonourContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	g := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _ = g.ListPeriodBranches(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ListPeriodBranches ignored the cancelled context")
	}
}

// TestGitLabPublisher_MRBodyNamesProjectsNotFilenames: the MR is what
// an operator reads before merging. Printing a rollup's filename under
// a heading that says "Project(s)" put
// "combined--epics-acme--7d--customer.md" there.
func TestGitLabPublisher_MRBodyNamesProjectsNotFilenames(t *testing.T) {
	var mrBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/branches/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"summarizer/2026-W42"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/repository/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v4/projects/group%2Freports/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		mrBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iid":1,"web_url":"https://gitlab.example/mr/1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")
	_, err := g.Publish(context.Background(), Request{
		Name:    "combined--epics-acme--7d--customer.md",
		Kind:    KindRollup,
		Period:  "2026-W42",
		Title:   "epics rollup for ACME customer",
		Project: "", // rollups have no single project
		Public:  true,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if strings.Contains(mrBody, "combined--epics-acme") {
		t.Errorf("MR body put a filename under the project heading: %s", mrBody)
	}
	if !strings.Contains(mrBody, "Period") {
		t.Errorf("MR body lost the period line: %s", mrBody)
	}
}
