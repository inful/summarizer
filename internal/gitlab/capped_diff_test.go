package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCappedDiff_TruncatesDuringDecode: the diff cap must bound the
// allocation, not just the result. Decoding `Diff` into a plain string
// materialises the whole multi-megabyte body and only then truncates, so
// a vendor-bump MR cost its full payload in RAM per fetch — times
// changeConcurrency concurrent workers, on every run including the
// default --include-diffs=off one that never reads the text.
func TestCappedDiff_TruncatesDuringDecode(t *testing.T) {
	huge := strings.Repeat("+this is a diff line\n", 200000) // ~5 MB
	var decodedLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"changes": []map[string]any{{
				"old_path": "go.mod", "new_path": "go.mod",
				"additions": 1, "deletions": 0, "diff": huge,
			}},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-x", 5*time.Second)

	_, chunks, err := c.FetchMRDetail(context.Background(), "g/p", 1)
	if err != nil {
		t.Fatalf("FetchMRDetail: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	got := chunks[0].Diff
	decodedLen = len([]rune(got))
	if decodedLen > maxDiffRunesPerFile+len("...[truncated]") {
		t.Errorf("diff kept %d runes, over the cap of %d", decodedLen, maxDiffRunesPerFile)
	}
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Error("a truncated diff must say so")
	}
	// The head of the diff survives, so the prompt still has content.
	if !strings.HasPrefix(got, "+this is a diff line") {
		t.Errorf("truncation dropped the head of the diff: %q", got[:40])
	}
}

// TestCappedDiff_PreservesMultiByteRunes: the cap counts runes, so a
// multi-byte diff must not be cut mid-character.
func TestCappedDiff_PreservesMultiByteRunes(t *testing.T) {
	body := strings.Repeat("ø", maxDiffRunesPerFile*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"changes": []map[string]any{{
				"old_path": "a", "new_path": "a", "diff": body,
			}},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-x", 5*time.Second)

	_, chunks, err := c.FetchMRDetail(context.Background(), "g/p", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range chunks[0].Diff {
		if r == '�' {
			t.Fatalf("replacement char at rune %d — truncation split a multi-byte rune", i)
		}
	}
}

// TestCappedDiff_AggregateUnaffected: the cap is on the body only. The
// "+N/-M in K files" line on every prompt comes from additions and
// deletions, which must survive intact.
func TestCappedDiff_AggregateUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"changes": []map[string]any{
				{"old_path": "a", "new_path": "a", "additions": 412, "deletions": 89, "diff": strings.Repeat("x", 5000)},
				{"old_path": "b", "new_path": "b", "additions": 3, "deletions": 1, "diff": "y"},
			},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-x", 5*time.Second)

	sum, _, err := c.FetchMRDetail(context.Background(), "g/p", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Additions != 415 || sum.Deletions != 90 || sum.Files != 2 {
		t.Errorf("aggregate corrupted by the diff cap: %+v", sum)
	}
}
