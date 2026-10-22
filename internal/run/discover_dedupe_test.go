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

// oneProjectServer serves a single project, `team-a/payments`, from
// BOTH the group scan and the topic scan — the composition the README
// documents at "summarizer run --discover-by-topic --discover-group".
func oneProjectServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"path_with_namespace":"team-a/payments","default_branch":"main","topics":["summarize"],"last_activity_at":"2026-10-19T00:00:00Z"}]`))
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound) // no opt-out file
		case r.URL.Query().Get("topic") != "", r.URL.Query().Get("last_activity_after") != "":
			// list endpoint
			_, _ = w.Write([]byte(`[{"id":1,"path_with_namespace":"team-a/payments","default_branch":"main","topics":["summarize"],"last_activity_at":"2026-10-19T00:00:00Z"}]`))
		case len(r.URL.Query()) > 0:
			// activity probe: one row ⇒ active
			_, _ = w.Write([]byte(`[{"iid":1}]`))
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main","topics":["summarize"]}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveProjects_OneProjectPerRunAcrossDiscoverySources: the two
// discovery loops compose (README, "run --discover-by-topic
// --discover-group"), and a project both of them surface was admitted
// twice. Each admission costs a full pair of LLM calls and adds a
// second, identical section to the cross-project rollup.
func TestResolveProjects_OneProjectPerRunAcrossDiscoverySources(t *testing.T) {
	srv := oneProjectServer(t)
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	list, disc, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoveredGroups: []string{"team-a"},
		DiscoverByTopic:  true,
		Since:            weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(list) != 1 || list[0] != "team-a/payments" {
		t.Fatalf("admitted %d times, want once: %v", len(list), list)
	}
	if !disc["team-a/payments"] {
		t.Error("the surviving entry should stay marked as discovered")
	}
}

// TestResolveProjects_DuplicateExplicitProjectRunsOnce: the same path
// twice on the command line (`--project a --project a`, or `--project
// a,a`) cost two runs and two rollup sections.
func TestResolveProjects_DuplicateExplicitProjectRunsOnce(t *testing.T) {
	srv := oneProjectServer(t)
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	list, _, _, err := resolveProjects(context.Background(), gc, Options{
		Projects: []string{"team-a/payments", "team-a/payments"},
		Since:    weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("admitted %d times, want once: %v", len(list), list)
	}
}
