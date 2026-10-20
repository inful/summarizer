package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchProjectSummary_HappyPath: GET /projects/:id returns the
// project's path, last_activity_at, and (for D5) its topic list.
// Topics flow through to the Classifier without re-fetching.
func TestFetchProjectSummary_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.EscapedPath(), "/projects/group%2Fpayments") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 1,
			"path_with_namespace": "group/payments",
			"last_activity_at": "2026-10-19T00:00:00Z",
			"topics": ["summarizer", "ruby", "kubernetes"]
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectSummary(context.Background(), "group/payments")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Path != "group/payments" {
		t.Errorf("path: %q", got.Path)
	}
	wantTopics := []string{"summarizer", "ruby", "kubernetes"}
	if len(got.Topics) != len(wantTopics) {
		t.Errorf("topics: want %v, got %v", wantTopics, got.Topics)
	}
	for i, want := range wantTopics {
		if got.Topics[i] != want {
			t.Errorf("topic[%d]: want %q, got %q", i, want, got.Topics[i])
		}
	}
}

// TestFetchProjectSummary_NoTopics: a project without topics returns
// an empty slice, not nil-or-missing. The classifier treats both
// the same way (default safe) so the empty-vs-nil distinction is
// about test convenience, not policy semantics.
func TestFetchProjectSummary_NoTopics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "path_with_namespace": "group/quiet", "last_activity_at": "2026-10-19T00:00:00Z", "topics": []}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectSummary(context.Background(), "group/quiet")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("summary should not be nil")
	}
	if got.Topics == nil {
		t.Errorf("topics should be empty slice, not nil")
	}
	if len(got.Topics) != 0 {
		t.Errorf("topics should be empty, got %v", got.Topics)
	}
}

// TestFetchProjectSummary_404DegradesToNil: a moved/renamed project
// returns (nil, nil) so the pipeline can skip classification rather
// than fail the run. Mirrors the soft-degrade contract used by
// other GitLab endpoints.
func TestFetchProjectSummary_404DegradesToNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectSummary(context.Background(), "group/missing")
	if err != nil {
		t.Fatalf("404 should NOT error: %v", err)
	}
	if got != nil {
		t.Errorf("404 should return nil summary")
	}
}
