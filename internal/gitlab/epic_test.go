package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEpic_UnmarshalShape: GitLab returns epics with nullable and
// optional fields summarizer does not model. Pins the JSON shape the
// scanner actually reads so an upstream field rename fails here first.
func TestEpic_UnmarshalShape(t *testing.T) {
	body := `{
		"id": 1, "iid": 7,
		"title": "Deploy v2 architecture",
		"description": "Plan for the v2 cutover",
		"state": "opened",
		"confidential": true,
		"web_url": "https://gitlab.example.com/groups/infra/-/epics/7",
		"start_date": "2026-09-01",
		"due_date": "2026-12-31",
		"created_at": "2026-08-15T10:00:00Z",
		"updated_at": "2026-10-19T12:00:00Z",
		"closed_at": null
	}`
	var e Epic
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.IID != 7 || e.Title != "Deploy v2 architecture" || e.State != "opened" {
		t.Errorf("core fields not decoded: %+v", e)
	}
	if !e.UpdatedAt.Equal(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("updated_at: got %v", e.UpdatedAt)
	}
	if !e.Confidential {
		t.Error("confidential must decode; the customer epic rollup filters on it")
	}
	if e.ClosedAt != nil {
		t.Errorf("closed_at null should stay nil: %v", e.ClosedAt)
	}
}

// TestFetchEpics_HappyPath: server returns a paginated list of epics.
// We expect to walk both pages and accumulate every epic.
func TestFetchEpics_HappyPath(t *testing.T) {
	page1 := `[
		{"iid": 1, "title": "Deploy v2", "state": "opened", "web_url": "u/1",
		 "created_at": "2026-08-01T00:00:00Z", "updated_at": "2026-10-19T00:00:00Z"},
		{"iid": 2, "title": "Customer login refresh", "state": "opened", "web_url": "u/2",
		 "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-10-18T00:00:00Z"}
	]`
	page2 := `[
		{"iid": 3, "title": "SLO enforcement", "state": "closed", "web_url": "u/3",
		 "created_at": "2026-07-01T00:00:00Z", "updated_at": "2026-10-10T00:00:00Z",
		 "closed_at": "2026-10-10T00:00:00Z"}
	]`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/infra/epics", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "2":
			w.Header().Set("X-Next-Page", "") // terminal page
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
	got, err := c.FetchEpics(context.Background(), "infra")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 epics across two pages, got %d", len(got))
	}
	if got[0].Title != "Deploy v2" || got[2].State != "closed" {
		t.Errorf("epic content not decoded: %+v", got)
	}
}

// TestFetchEpics_FreeTierDegrades: GitLab Free / no-premium returns 403
// from /epics. The summarizer caller treats epics as optional, so we
// surface a soft (nil, nil) — the pipeline skips the epic section
// entirely and emits a one-line stderr note. Real error is reserved
// for genuine transport / 5xx failures.
func TestFetchEpics_FreeTierDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchEpics(context.Background(), "infra")
	if err != nil {
		t.Fatalf("free-tier 403 should NOT error; got %v", err)
	}
	if got != nil {
		t.Errorf("free-tier 403 should return nil slice, got %v", got)
	}
}

// TestFetchEpics_NotFoundDegrades: 404 (URL wrong / group doesn't
// exist) is also a graceful-degrade signal. Same path as free-tier.
func TestFetchEpics_NotFoundDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchEpics(context.Background(), "infra")
	if err != nil {
		t.Fatalf("404 should NOT error: %v", err)
	}
	if got != nil {
		t.Errorf("404 should return nil slice, got %v", got)
	}
}

// TestFetchEpics_AuthFailureHardErrors: 401 IS a hard error — the
// token is wrong, the operator needs to know.
func TestFetchEpics_AuthFailureHardErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "401", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	_, err := c.FetchEpics(context.Background(), "infra")
	if err == nil {
		t.Fatal("401 should hard-error")
	}
}

// TestFetchEpicIssues_HappyPath: fetches issues linked to one epic.
// Each is a full Issue record.
func TestFetchEpicIssues_HappyPath(t *testing.T) {
	body := `[
		{"iid": 100, "title": "Wire up new auth API", "state": "closed",
		 "author": {"username": "alice"}, "web_url": "u/i/100",
		 "created_at": "2026-08-01T00:00:00Z", "updated_at": "2026-10-18T00:00:00Z",
		 "closed_at": "2026-10-18T00:00:00Z", "labels": [], "user_notes_count": 5},
		{"iid": 101, "title": "Roll out token refresh", "state": "opened",
		 "author": {"username": "bob"}, "web_url": "u/i/101",
		 "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-10-19T00:00:00Z",
		 "labels": [], "user_notes_count": 1}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/infra/epics/7/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchEpicIssues(context.Background(), "infra", 7)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(got))
	}
	if got[0].IID != 100 || got[1].Title != "Roll out token refresh" {
		t.Errorf("issue content off: %+v", got)
	}
}

// TestFetchProjectNamespace_GroupPath: the project's namespace.full_path
// is what we hit /epics against. User-namespace projects (kind="user")
// have no group, so we surface that explicitly so the pipeline skips
// epic-fanout without a 404 round-trip.
func TestFetchProjectNamespace_GroupPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Freports", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{
			"id": 12,
			"path_with_namespace": "group/reports",
			"namespace": {
				"id": 99, "kind": "group",
				"path": "reports", "full_path": "group/reports",
				"name": "Reports", "avatar_url": null
			}
		}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	ns, err := c.FetchProjectNamespace(context.Background(), "group/reports")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if ns.Kind != "group" || ns.FullPath != "group/reports" {
		t.Errorf("namespace not parsed: %+v", ns)
	}
}

// TestFetchProjectNamespace_UserNamespace: a project under a user
// namespace has no group and therefore no epics. The pipeline
// short-circuits the epic-fetch path on a non-group namespace kind.
func TestFetchProjectNamespace_UserNamespace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/someuser%2Fpersonal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"namespace": {"kind": "user", "full_path": "someuser"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	ns, err := c.FetchProjectNamespace(context.Background(), "someuser/personal")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if ns.Kind != "user" {
		t.Errorf("user namespace should report kind=user: %+v", ns)
	}
	if strings.Contains(ns.FullPath, "/") {
		// user namespaces have a single path segment; grouping would
		// accidentally treat the bare username as a group path.
		t.Errorf("user namespace path should not contain '/': %+v", ns)
	}
}

// TestIssue_ConfidentialDecodes: the customer epic rollup has always
// honoured Epic.Confidential; Issue carried no equivalent field at all,
// so a confidential issue was indistinguishable from an ordinary one and
// its title reached a document published public: true. The flag is now
// decoded, and this pins that — a missing json tag would silently
// default it to false and reopen the leak.
func TestIssue_ConfidentialDecodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"confidential", `[{"iid":1,"title":"ACME pricing renegotiation — INTERNAL ONLY","confidential":true}]`, true},
		{"ordinary", `[{"iid":2,"title":"shipped it","confidential":false}]`, false},
		// GitLab omits the key on some response shapes. Absent must
		// decode as false, not error.
		{"absent", `[{"iid":3,"title":"shipped it"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Issue
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d issues, want 1", len(got))
			}
			if got[0].Confidential != tc.want {
				t.Errorf("Confidential = %v, want %v", got[0].Confidential, tc.want)
			}
		})
	}
}
