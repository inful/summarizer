package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// TestResolveProjects_DiscoverActive_IncludesActiveExcludesOptOut:
// the default discovery path. Active projects join the run without a
// topic; a project carrying .summaryignore is dropped even though it
// is active and even though the discovery signal is activity, not an
// operator's explicit --project listing.
func TestResolveProjects_DiscoverActive_IncludesActiveExcludesOptOut(t *testing.T) {
	mux := http.NewServeMux()
	// The /projects list is the activity scan: last_activity_after
	// is the server-side filter.
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last_activity_after") == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id": 1, "path_with_namespace": "team-a/api",       "last_activity_at": "2026-10-19T00:00:00Z", "topics": []},
			{"id": 2, "path_with_namespace": "team-b/private",   "last_activity_at": "2026-10-19T00:00:00Z", "topics": []},
			{"id": 3, "path_with_namespace": "team-c/quiet",     "last_activity_at": "2026-10-01T00:00:00Z", "topics": []}
		]`))
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/repository/files/"):
			// Only team-b/private carries the opt-out file.
			if !strings.Contains(path, "private") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"file_name":".summaryignore","file_path":".summaryignore","default_branch":"main"}`))
		case strings.Contains(path, "/merge_requests"), strings.Contains(path, "/issues"):
			// Activity gate: team-c/quiet has nothing in the window.
			if strings.Contains(path, "quiet") {
				w.Header().Set("X-Next-Page", "")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
				return
			}
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"iid": 1}]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main","topics":[]}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	proj, disc, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		Since:          7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"team-a/api"}
	if len(proj) != len(want) {
		t.Fatalf("want %v, got %v", want, proj)
	}
	for i, w := range want {
		if proj[i] != w {
			t.Errorf("project %d: want %q, got %q", i, w, proj[i])
		}
		if !disc[w] {
			t.Errorf("project %q must be marked as discovered", w)
		}
	}
}
