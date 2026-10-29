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

// TestSetMaxDiffRunes_ChangesTheCap: the per-file diff cap is the one
// budget that cannot be handed to the client, because it is enforced
// inside a JSON unmarshaller the decoder constructs. It is a
// package-level setting instead, written once at startup — and it has
// to actually take effect, or the config key is a lie.
func TestSetMaxDiffRunes_ChangesTheCap(t *testing.T) {
	original := maxDiffRunesPerFile
	t.Cleanup(func() { maxDiffRunesPerFile = original })

	body := strings.Repeat("+a diff line\n", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"changes": []map[string]any{{
				"old_path": "go.mod", "new_path": "go.mod",
				"additions": 1, "deletions": 0, "diff": body,
			}},
		})
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-x", 5*time.Second)

	fetch := func() string {
		t.Helper()
		_, chunks, err := c.FetchMRDetail(context.Background(), "g/p", 1)
		if err != nil {
			t.Fatalf("FetchMRDetail: %v", err)
		}
		if len(chunks) != 1 {
			t.Fatalf("want 1 chunk, got %d", len(chunks))
		}
		return chunks[0].Diff
	}

	SetMaxDiffRunes(50)
	small := fetch()
	if n := len([]rune(small)); n > 50+len("...[truncated]") {
		t.Errorf("diff kept %d runes, over the configured cap of 50", n)
	}

	SetMaxDiffRunes(5000)
	big := fetch()
	if len([]rune(big)) <= len([]rune(small)) {
		t.Errorf("raising the cap did not keep more of the diff: %d runes vs %d",
			len([]rune(big)), len([]rune(small)))
	}
}

// TestSetMaxDiffRunes_IgnoresNonPositive: a misconfigured limit must
// not truncate every diff to nothing. Zero means "unset" and leaves the
// default in place.
func TestSetMaxDiffRunes_IgnoresNonPositive(t *testing.T) {
	original := maxDiffRunesPerFile
	t.Cleanup(func() { maxDiffRunesPerFile = original })

	SetMaxDiffRunes(0)
	if maxDiffRunesPerFile != original {
		t.Errorf("a zero cap changed the limit to %d", maxDiffRunesPerFile)
	}
	SetMaxDiffRunes(-5)
	if maxDiffRunesPerFile != original {
		t.Errorf("a negative cap changed the limit to %d", maxDiffRunesPerFile)
	}
}
