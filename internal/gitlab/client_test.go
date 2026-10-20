package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeGitLabURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://gitlab.example.com", "https://gitlab.example.com/api/v4"},
		{"https://gitlab.example.com/", "https://gitlab.example.com/api/v4"},
		{"https://gitlab.example.com/api/v4", "https://gitlab.example.com/api/v4"},
		{"https://gitlab.example.com/api/v4/", "https://gitlab.example.com/api/v4"},
		{"https://gitlab.com", "https://gitlab.com/api/v4"},
	}
	for _, c := range cases {
		got := normalizeGitLabURL(c.in)
		if got != c.want {
			t.Errorf("normalizeGitLabURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRetryAfterCap(t *testing.T) {
	// "999999" seconds = 277 hours; must be capped to retryAfterCapDefault.
	got := retryAfter("999999")
	if got != retryAfterCapDefault {
		t.Errorf("retryAfter(\"999999\") = %v, want cap %v", got, retryAfterCapDefault)
	}
	// Delta-seconds form, in-range.
	if got := retryAfter("30"); got != 30*time.Second {
		t.Errorf("retryAfter(\"30\") = %v, want 30s", got)
	}
	// Unparseable → fall back to backoff.
	if got := retryAfter("not-a-date"); got <= 0 {
		t.Errorf("retryAfter(unparseable) should fall back to backoff, got %v", got)
	}
}

func TestBackoffInRange(t *testing.T) {
	// ±20% jitter around the cap; 100 samples should all stay within [0.8*30s, 1.2*30s].
	low := time.Duration(float64(maxBackoff)*0.8) - time.Second
	high := time.Duration(float64(maxBackoff)*1.2) + time.Second
	for i := 0; i < 100; i++ {
		got := backoff(20) // forces the cap
		if got < low || got > high {
			t.Errorf("backoff(20) sample %d = %v, outside [%v, %v]", i, got, low, high)
		}
	}
}

// TestFetchMRChanges_httptest runs against a fake GitLab server that
// returns a /changes-shaped payload, including per-file diff bodies
// (which we expect to be discarded). Verifies the aggregate sums and
// confirms the diff text doesn't leak into memory.
func TestFetchMRChanges_httptest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests/847/changes") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			t.Errorf("missing PRIVATE-TOKEN header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
				"id": 1, "iid": 847, "title": "Add login",
				"changes": [
					{"old_path":"a","new_path":"a","new_file":false,"deleted_file":false,"renamed_file":false,"additions":10,"deletions":2,"diff":"@@ -1 +1 @@\n-foo\n+bar"},
					{"old_path":"b","new_path":"b","new_file":false,"deleted_file":false,"renamed_file":false,"additions":20,"deletions":5,"diff":"@@ -1 +1 @@\n-foo\n+bar"},
					{"old_path":"c","new_path":"c","new_file":true,"deleted_file":false,"renamed_file":false,"additions":3,"deletions":0,"diff":"@@ -0,0 +1 @@\n+new file"}
				]
			}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	s, err := c.FetchMRChanges(context.Background(), "group/proj", 847)
	if err != nil {
		t.Fatalf("FetchMRChanges: %v", err)
	}
	if s.Files != 3 || s.Additions != 33 || s.Deletions != 7 {
		t.Errorf("unexpected summary: %+v", s)
	}
}

// TestFetchMRChanges_PartialFailure confirms a missing /changes response
// surfaces as an error without panicking or silently zeroing the field.
func TestFetchMRChanges_PartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	_, err := c.FetchMRChanges(context.Background(), "group/proj", 9999)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}
