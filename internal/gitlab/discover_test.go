package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchGroupProjects_HappyPath: lists projects in a group across
// two pagination pages. The summary is what summarizer needs for
// discovery — full Project details (description, topics, avatar_url)
// would be wasted bandwidth.
func TestFetchGroupProjects_HappyPath(t *testing.T) {
	page1 := `[
		{"id": 1, "path_with_namespace": "infra/payments", "last_activity_at": "2026-10-18T12:00:00Z", "topics": ["summarize"]},
		{"id": 2, "path_with_namespace": "infra/auth",     "last_activity_at": "2026-09-04T12:00:00Z"}
	]`
	page2 := `[
		{"id": 3, "path_with_namespace": "infra/search",   "last_activity_at": "2026-10-19T08:00:00Z", "topics": ["summarize-team-only", "billing"]}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/infra/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "2":
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(page2))
		default:
			w.Header().Set("X-Next-Page", "2")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(page1))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchGroupProjects(context.Background(), "infra", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 projects across two pages, got %d", len(got))
	}
	if got[0].Path != "infra/payments" || got[0].ID != 1 {
		t.Errorf("project 0 wrong: %+v", got[0])
	}
	if got[2].Path != "infra/search" {
		t.Errorf("project 2 wrong: %+v", got[2])
	}
	// Topics field flows through: project 0 has [summarize],
	// project 1 has [], project 2 has [summarize-team-only, billing].
	if len(got[0].Topics) != 1 || got[0].Topics[0] != "summarize" {
		t.Errorf("project 0 topics: want [summarize], got %+v", got[0].Topics)
	}
	if len(got[1].Topics) != 0 {
		t.Errorf("project 1 topics: want [], got %+v", got[1].Topics)
	}
	if len(got[2].Topics) != 2 || got[2].Topics[0] != "summarize-team-only" {
		t.Errorf("project 2 topics: want [summarize-team-only billing], got %+v", got[2].Topics)
	}
}

// TestFetchGroupProjects_Empty: a group with no projects returns an
// empty slice (not nil) — caller ranges over the result without
// panic. Distinguishing from an error: this is normal, no projects
// at all.
func TestFetchGroupProjects_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchGroupProjects(context.Background(), "empty-group", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty group should return empty slice, got %+v", got)
	}
}

// TestProbeProjectActivity_HasMRs: when the project has at least
// one MR updated within the window, the probe returns true. Use
// updated_after=since is what makes the MR list activity-filtered.
func TestProbeProjectActivity_HasMRs(t *testing.T) {
	since := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Fpayments/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("updated_after"); got == "" {
			t.Errorf("probe must use updated_after: query=%v", r.URL.RawQuery)
		}
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"iid": 12, "title": "merge auth"},
			{"iid": 13, "title": "fix typo"}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	active, err := c.ProbeProjectActivity(context.Background(), "group/payments", since)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !active {
		t.Errorf("project with MRs should be active")
	}
}

// TestProbeProjectActivity_NoMRs: when no MRs were updated in the
// window, the project is inactive for discovery purposes. Even with
// last_activity_at recent (a CI run, a wiki edit), the engineering-
// work signal is what counts.
func TestProbeProjectActivity_NoMRs(t *testing.T) {
	since := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	active, err := c.ProbeProjectActivity(context.Background(), "group/quiet", since)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if active {
		t.Errorf("project with no MRs in window should be inactive")
	}
}

// Activity-gate tests live in activity_test.go: the gate is now
// "MRs OR issues" rather than MRs only, and the old single-probe
// test fixtures were replaced with a per-project matrix fake.
