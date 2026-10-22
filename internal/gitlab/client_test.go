package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// TestFetchMRDetail_httptest runs against a fake GitLab server that
// returns a /changes-shaped payload including per-file diff bodies.
// Verifies the aggregate sums AND that the truncated diff chunks come
// back from the same single call — the point of collapsing
// FetchMRChanges and FetchMRDetail into one method.
func TestFetchMRDetail_httptest(t *testing.T) {
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
	s, chunks, err := c.FetchMRDetail(context.Background(), "group/proj", 847)
	if err != nil {
		t.Fatalf("FetchMRDetail: %v", err)
	}
	if s.Files != 3 || s.Additions != 33 || s.Deletions != 7 {
		t.Errorf("unexpected summary: %+v", s)
	}
	if len(chunks) != 3 {
		t.Fatalf("want 3 diff chunks from the same call, got %d", len(chunks))
	}
	if chunks[0].Diff == "" || !strings.Contains(chunks[0].Diff, "+bar") {
		t.Errorf("diff body missing or truncated unexpectedly: %q", chunks[0].Diff)
	}
	if chunks[0].Path != "a" {
		t.Errorf("chunk path: got %q, want a", chunks[0].Path)
	}
}

// TestFetchMRDetail_TruncatesLongDiff: GitLab can return multi-MB diffs
// for big refactors; each chunk's body must be capped so one MR can't
// exhaust memory before the prompt formatter gets a say.
func TestFetchMRDetail_TruncatesLongDiff(t *testing.T) {
	huge := strings.Repeat("x", maxDiffRunesPerFile*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changes":[{"old_path":"big","new_path":"big",` +
			`"additions":1,"deletions":0,"diff":` + fmt.Sprintf("%q", huge) + `}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	_, chunks, err := c.FetchMRDetail(context.Background(), "group/proj", 1)
	if err != nil {
		t.Fatalf("FetchMRDetail: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if got := len([]rune(chunks[0].Diff)); got > maxDiffRunesPerFile+len("...[truncated]") {
		t.Errorf("diff not capped: %d runes", got)
	}
	if !strings.HasSuffix(chunks[0].Diff, "...[truncated]") {
		t.Error("truncation marker missing")
	}
}

// TestFetchMRDetail_PartialFailure confirms a missing /changes response
// surfaces as an error without panicking or silently zeroing the field.
func TestFetchMRDetail_PartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "glpat-test", 5*time.Second)
	_, _, err := c.FetchMRDetail(context.Background(), "group/proj", 9999)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

// TestCappedDiff_TruncationDoesNotScaleWithTheBody: the cap on the
// retained text has always worked. What it did NOT do was bound the
// transient cost of applying it — `[]rune(s)` allocates four bytes per
// byte, so capping a 40 MB diff allocated 160 MB to count the
// characters it was about to discard. Measured on ASCII:
//
//	wire payload            :  40.0 MB
//	retained Diff           :   0.001 MB
//	allocated while decoding: 240.0 MB   (6.0x)
//
// This pins the 4x that remains — `string(b)` and strconv.Unquote — so
// putting a third copy back is a test failure rather than a Sunday-night
// surprise on a machine that is also running the cron.
func TestCappedDiff_TruncationDoesNotScaleWithTheBody(t *testing.T) {
	const mb = 1 << 20
	raw := strings.Repeat("a", 40*mb)
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := append(append([]byte(`{"diff":`), body...), '}')

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var out struct {
		Diff cappedDiff `json:"diff"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	if got, want := len(out.Diff.Text), maxDiffRunesPerFile+len("...[truncated]"); got != want {
		t.Errorf("retained %d bytes, want %d", got, want)
	}
	allocated := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(payload))
	if allocated > 4.5 {
		t.Errorf("decoding allocated %.1fx the wire payload; the two unavoidable "+
			"copies (string(b) and Unquote) come to about 4x", allocated)
	}
	t.Logf("wire %d MB, retained %d B, transient %.1fx",
		len(payload)/mb, len(out.Diff.Text), allocated)
}
