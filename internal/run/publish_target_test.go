package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// reportTargetServer serves an instance holding one real project and
// one reports repo. Both are active, so only the publish-target rule
// can tell them apart.
func reportTargetServer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last_activity_after") == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id": 1, "path_with_namespace": "it/Drift",     "last_activity_at": "2026-10-19T00:00:00Z", "topics": []},
			{"id": 2, "path_with_namespace": "sfb/reports", "last_activity_at": "2026-10-19T00:00:00Z", "topics": []}
		]`))
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(path, "/merge_requests"),
			strings.Contains(path, "/issues"),
			strings.Contains(path, "/repository/commits"):
			// Everything is active, including the reports repo.
			_, _ = w.Write([]byte(`[{"iid": 1}]`))
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main","topics":[]}`))
		}
	})
	return mux
}

func reportTargetClient(t *testing.T) *gitlab.Client {
	t.Helper()
	srv := httptest.NewServer(reportTargetServer())
	t.Cleanup(srv.Close)
	return gitlab.New(srv.URL, "glpat-test", 5*time.Second)
}

// TestResolveProjects_SkipsThePublishTarget: the reports project is
// not a project anyone wants summarised. Its merge requests are
// summarizer's own publishing MRs, so leaving it in spends an LLM call
// every week describing the previous week's reports — and it is active
// every single week, so the activity gate never filters it.
func TestResolveProjects_SkipsThePublishTarget(t *testing.T) {
	proj, _, _, err := resolveProjects(context.Background(), reportTargetClient(t), Options{
		DiscoverActive: true,
		PublishTarget:  "sfb/reports",
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if slices.Contains(proj, "sfb/reports") {
		t.Errorf("the publish target must not be summarised, got %v", proj)
	}
	if !slices.Contains(proj, "it/Drift") {
		t.Errorf("the real project should still run, got %v", proj)
	}
}

// TestResolveProjects_ExplicitPublishTargetStillRuns: the exclusion is
// a default, not a policy. `--project sfb/reports` is the operator
// overriding it, same authority as the publish.project line that
// triggered the skip in the first place.
func TestResolveProjects_ExplicitPublishTargetStillRuns(t *testing.T) {
	proj, _, _, err := resolveProjects(context.Background(), reportTargetClient(t), Options{
		DiscoverActive: true,
		PublishTarget:  "sfb/reports",
		Projects:       []string{"sfb/reports"},
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !slices.Contains(proj, "sfb/reports") {
		t.Errorf("an explicitly listed publish target should run, got %v", proj)
	}
}

// TestResolveProjects_NoPublishTargetSummarisesEverything: the local
// publisher writes to disk, so there is no project to skip and
// behaviour must be unchanged.
func TestResolveProjects_NoPublishTargetSummarisesEverything(t *testing.T) {
	proj, _, _, err := resolveProjects(context.Background(), reportTargetClient(t), Options{
		DiscoverActive: true,
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !slices.Contains(proj, "sfb/reports") {
		t.Errorf("with no publish target nothing should be skipped, got %v", proj)
	}
}
