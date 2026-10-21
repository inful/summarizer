package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pagedServer serves `pages` pages of a JSON array, wiring X-Next-Page
// the way GitLab does: the last page sends an empty header.
func pagedServer(t *testing.T, path string, pages []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != path {
			http.Error(w, "unexpected path "+r.URL.EscapedPath(), http.StatusNotFound)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.URL.Query().Get("page"), "%d", &n); err != nil || n < 1 {
			n = 1
		}
		if n > len(pages) {
			http.Error(w, "past the end", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if n < len(pages) {
			w.Header().Set("X-Next-Page", fmt.Sprint(n+1))
		} else {
			w.Header().Set("X-Next-Page", "")
		}
		_, _ = w.Write([]byte(pages[n-1]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGetAll_WalksEveryPage: getAll is the loop behind every list
// endpoint. A project with more than 100 commits in the window is the
// exact case that matters and the one no existing test covered — the
// old fakes all returned a single empty-terminated page.
func TestGetAll_WalksEveryPage(t *testing.T) {
	page := func(from, to int) string {
		out := "["
		for i := from; i <= to; i++ {
			if i > from {
				out += ","
			}
			out += fmt.Sprintf(`{"id":%d,"path_with_namespace":"g/p%d","last_activity_at":"2026-10-19T00:00:00Z"}`, i, i)
		}
		return out + "]"
	}
	srv := pagedServer(t, "/api/v4/groups/g/projects", []string{page(1, 2), page(3, 4), page(5, 5)})
	c := New(srv.URL, "glpat-test", 5*time.Second)

	got, err := c.FetchGroupProjects(context.Background(), "g", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d projects across 3 pages, want 5: %+v", len(got), got)
	}
	for i, p := range got {
		if want := fmt.Sprintf("g/p%d", i+1); p.Path != want {
			t.Errorf("page-order broken at %d: got %q, want %q", i, p.Path, want)
		}
	}
}

// TestGetAll_StopsOnEmptyPageEvenWithNextHeader: some GitLab proxies
// keep sending X-Next-Page on an empty page. An unguarded loop would
// spin forever against such a server.
func TestGetAll_StopsOnEmptyPageEvenWithNextHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"path_with_namespace":"g/p1","last_activity_at":"2026-10-19T00:00:00Z"}]`))
			return
		}
		// Empty page but the header still claims more.
		w.Header().Set("X-Next-Page", "3")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 2*time.Second)

	done := make(chan struct{})
	var n int
	var err error
	go func() {
		var out []ProjectSummary
		out, err = c.FetchGroupProjects(context.Background(), "g", false)
		n = len(out)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pagination loop did not terminate on an empty page")
	}
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d projects, want 1 (the single non-empty page)", n)
	}
}

// TestFetchMRs_PaginatesAndFilters: the real per-project path — the
// updated_after filter must survive pagination and MRs must arrive in
// page order.
func TestFetchMRs_PaginatesAndFilters(t *testing.T) {
	var seenSince []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("updated_after"); got != "" {
			seenSince = append(seenSince, got)
		}
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"iid":2,"title":"second"}]`))
			return
		}
		w.Header().Set("X-Next-Page", "2")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"iid":1,"title":"first"}]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	since := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	got, err := c.FetchMRs(context.Background(), "g/p", since)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 || got[0].IID != 1 || got[1].IID != 2 {
		t.Fatalf("want MRs 1,2 across two pages, got %+v", got)
	}
	for _, s := range seenSince {
		if s != "2026-10-13T00:00:00Z" {
			t.Errorf("updated_after lost across pagination: %q", s)
		}
	}
}

// TestFetchCommits_Paginates: the busiest endpoint in practice.
func TestFetchCommits_Paginates(t *testing.T) {
	srv := pagedServer(t, "/api/v4/projects/g%2Fp/repository/commits", []string{
		`[{"short_id":"a","title":"one"}]`,
		`[{"short_id":"b","title":"two"}]`,
	})
	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchCommits(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 || got[0].ShortID != "a" || got[1].ShortID != "b" {
		t.Errorf("want commits a,b, got %+v", got)
	}
}

// TestFetchIssues_Paginates: same, for the issues endpoint.
func TestFetchIssues_Paginates(t *testing.T) {
	srv := pagedServer(t, "/api/v4/projects/g%2Fp/issues", []string{
		`[{"iid":10,"title":"one"}]`,
		`[{"iid":11,"title":"two"}]`,
	})
	c := New(srv.URL, "glpat-test", 5*time.Second)
	got, err := c.FetchIssues(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 || got[0].IID != 10 || got[1].IID != 11 {
		t.Errorf("want issues 10,11, got %+v", got)
	}
}
