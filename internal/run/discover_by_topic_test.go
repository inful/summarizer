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

// TestResolveProjects_DiscoverByTopic: --discover-by-topic with no
// --project and no --discover-group is enough to run. The server-wide
// scan crosses group boundaries, applies the same MR activity gate,
// marks everything as discovered, and classifies audience per D5.
func TestResolveProjects_DiscoverByTopic(t *testing.T) {
	active := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"iid": 1}]`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("topic") {
		case "summarize":
			_, _ = w.Write([]byte(`[
				{"id": 1, "path_with_namespace": "team-a/api",      "last_activity_at": "2026-10-19T00:00:00Z", "topics": ["summarize"]},
				{"id": 2, "path_with_namespace": "team-b/quiet",    "last_activity_at": "2026-10-01T00:00:00Z", "topics": ["summarize"]},
				{"id": 3, "path_with_namespace": "team-c/internal", "last_activity_at": "2026-10-19T00:00:00Z", "topics": ["summarize-team-only"]}
			]`))
		default:
			// The team-only topic pass would re-return id 3; the
			// client dedupes. Return it so the union path is
			// exercised end to end.
			_, _ = w.Write([]byte(`[
				{"id": 3, "path_with_namespace": "team-c/internal", "last_activity_at": "2026-10-19T00:00:00Z", "topics": ["summarize-team-only"]}
			]`))
		}
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "team-b/quiet") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		active(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	proj, disc, err := resolveProjects(context.Background(), gc, Options{
		DiscoverByTopic: true,
		Since:           7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// team-b/quiet has no MRs in the window → excluded.
	// team-c/internal IS active but carries summarize-team-only → still runs.
	want := []string{"team-a/api", "team-c/internal"}
	if len(proj) != len(want) {
		t.Fatalf("want %v, got %v", want, proj)
	}
	for i, w := range want {
		if proj[i] != w {
			t.Errorf("project %d: want %q, got %q", i, w, proj[i])
		}
		if !disc[proj[i]] {
			t.Errorf("%s should be marked discovered", proj[i])
		}
	}
}

// TestResolveProjects_DiscoverByTopicRespectsExplicit: an explicit
// --project that also carries the topic is deduped — it stays in the
// list once and is NOT marked as discovered, so the policy classifier
// treats it as explicit (both audiences, unless overridden).
func TestResolveProjects_DiscoverByTopicRespectsExplicit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 1, "path_with_namespace": "team-a/api", "last_activity_at": "2026-10-19T00:00:00Z", "topics": ["summarize-team-only"]}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	proj, disc, err := resolveProjects(context.Background(), gc, Options{
		Projects:        []string{"team-a/api"},
		DiscoverByTopic: true,
		Since:           7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(proj) != 1 || proj[0] != "team-a/api" {
		t.Fatalf("want exactly [team-a/api], got %v", proj)
	}
	if disc["team-a/api"] {
		t.Errorf("explicit listing must win over discovery, not be marked discovered")
	}
}
