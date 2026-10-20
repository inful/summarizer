package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchProjectsByTopic_UnionAcrossTopics: GitLab's ?topic= filter
// is AND — one call per topic, unioned by project ID. A project
// carrying both topics appears once. Result order is stable (first
// topic's order first, new appends after) so the run is reproducible.
func TestFetchProjectsByTopic_UnionAcrossTopics(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		topic := r.URL.Query().Get("topic")
		if topic == "" {
			t.Errorf("topic query param missing: %v", r.URL.RawQuery)
		}
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch topic {
		case "summarize":
			_, _ = w.Write([]byte(`[
				{"id": 1, "path_with_namespace": "a/one", "last_activity_at": "2026-10-18T12:00:00Z", "topics": ["summarize"]},
				{"id": 2, "path_with_namespace": "b/both", "last_activity_at": "2026-10-18T12:00:00Z", "topics": ["summarize","summarize-team-only"]}
			]`))
		case "summarize-team-only":
			_, _ = w.Write([]byte(`[
				{"id": 2, "path_with_namespace": "b/both", "last_activity_at": "2026-10-18T12:00:00Z", "topics": ["summarize","summarize-team-only"]},
				{"id": 3, "path_with_namespace": "c/three", "last_activity_at": "2026-10-19T12:00:00Z", "topics": ["summarize-team-only"]}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectsByTopic(context.Background(), []string{"summarize", "summarize-team-only"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 unique projects (id 2 deduped), got %d: %+v", len(got), got)
	}
	want := []string{"a/one", "b/both", "c/three"}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("project %d: want %q, got %q", i, w, got[i].Path)
		}
	}
	// Topics survive the union so the policy classifier can still see
	// the full list on the deduped entry.
	if len(got[1].Topics) != 2 {
		t.Errorf("deduped project should keep both topics, got %+v", got[1].Topics)
	}
}

// TestFetchProjectsByTopic_Paginates: a topic with more than 100
// projects spans pages. The X-Next-Page header drives the loop.
func TestFetchProjectsByTopic_Paginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("X-Next-Page", "")
			_, _ = w.Write([]byte(`[{"id": 101, "path_with_namespace": "g/p101", "last_activity_at": "2026-10-18T12:00:00Z"}]`))
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_, _ = w.Write([]byte(`[{"id": 100, "path_with_namespace": "g/p100", "last_activity_at": "2026-10-18T12:00:00Z"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectsByTopic(context.Background(), []string{"summarize"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 across pages, got %d", len(got))
	}
}

// TestFetchProjectsByTopic_EmptyTopicList: no topics means no API
// call and no results — the flag stays a no-op rather than fetching
// every project on the instance.
func TestFetchProjectsByTopic_EmptyTopicList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call expected for an empty topic list, got %s", r.URL.Path)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchProjectsByTopic(context.Background(), nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}
