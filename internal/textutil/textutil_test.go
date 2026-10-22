package textutil

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestTruncate_KeepsShortInput: anything at or under the cap comes back
// byte-identical, including the empty string.
func TestTruncate_KeepsShortInput(t *testing.T) {
	for _, s := range []string{"", "a", "exact-8", "12345678"} {
		if got := Truncate(s, 8); got != s {
			t.Errorf("Truncate(%q, 8) = %q, want unchanged", s, got)
		}
	}
}

// TestTruncate_CountsRunesNotBytes: the whole point. A byte-based cut
// lands mid-character and emits an invalid lead byte.
func TestTruncate_CountsRunesNotBytes(t *testing.T) {
	// "kundeøkning" has 11 runes, 12 bytes (ø is 2 bytes).
	s := "Fix for kundeøkning i Tyskland"
	for _, n := range []int{1, 10, 11, 13, 14, 20} {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d) = %q — invalid UTF-8", s, n, got)
		}
	}
	// Cut mid-ø (ø starts at rune 13, byte 13): the result must still
	// hold the complete character.
	got := Truncate(s, 14)
	if !strings.HasPrefix(got, "Fix for kundeø") {
		t.Errorf("Truncate(%q, 14) = %q, want prefix %q", s, got, "Fix for kundeø")
	}
}

// TestTruncate_EmojiSurvives: 4-byte runes are the other common case.
func TestTruncate_EmojiSurvives(t *testing.T) {
	s := "Deploy 🚀 rollout now"
	for n := 1; n <= utf8.RuneCountInString(s); n++ {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Errorf("Truncate(cap=%d) = %q — invalid UTF-8", n, got)
		}
	}
	if got := Truncate(s, 8); !strings.HasPrefix(got, "Deploy 🚀") {
		t.Errorf("Truncate(cap=8) = %q, want the emoji intact", got)
	}
}

// TestTruncate_RuneCountMatchesCap: the visible payload is exactly n
// runes plus the marker.
func TestTruncate_RuneCountMatchesCap(t *testing.T) {
	s := "abcdefghij"
	got := Truncate(s, 4)
	if utf8.RuneCountInString(got) != 4+utf8.RuneCountInString("...") {
		t.Errorf("Truncate(%q,4) = %q — payload is not exactly 4 runes + marker", s, got)
	}
}

// TestTruncate_NonPositiveCap: a zero or negative cap yields an empty
// string rather than panicking on r[:0] of an empty rune slice.
func TestTruncate_NonPositiveCap(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := Truncate("anything", n); got != "" {
			t.Errorf("Truncate(_, %d) = %q, want empty", n, got)
		}
	}
}

// TestRetryAfter: one copy for internal/gitlab, internal/llm and
// internal/publisher, which each had their own — and each carried the
// same defect, because all three multiplied seconds into a Duration
// before capping it.
//
// The case that matters is the one the old internal/gitlab test
// stopped short of. It asserted retryAfter("999999"), which is 999,999
// seconds — it fits in int64 nanoseconds with eleven orders of
// magnitude to spare, so it proves the cap works and says nothing
// about the boundary. Overflow starts around 9.2e9.
func TestRetryAfter(t *testing.T) {
	const cap = 60 * time.Second
	fallback := 1 * time.Second

	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"zero means retry now", "0", 0},
		{"in range", "30", 30 * time.Second},
		{"large but not overflowing", "999999", cap},
		// The overflow boundary: 9.2e9 seconds is ~292 years, which
		// time.Duration(n) * time.Second cannot hold.
		{"just past the overflow boundary", "9223372037", cap},
		{"far past it", "9999999999", cap},
		{"math.MaxInt64", "9223372036854775807", cap},
		{"missing header falls back", "", fallback},
		{"garbage falls back", "not-a-date", fallback},
		{"negative falls back", "-1", fallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RetryAfter(c.in, cap, fallback)
			if got != c.want {
				t.Errorf("RetryAfter(%q) = %v, want %v", c.in, got, c.want)
			}
			if got < 0 {
				t.Errorf("RetryAfter(%q) = %v; a negative wait is the bug this "+
					"function exists to prevent", c.in, got)
			}
		})
	}

	// An HTTP-date in the past is a wait of zero, not a negative one.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := RetryAfter(past, cap, fallback); got != 0 {
		t.Errorf("RetryAfter(past date) = %v, want 0", got)
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if got := RetryAfter(future, cap, fallback); got <= 0 || got > cap {
		t.Errorf("RetryAfter(future date) = %v, want (0, %v]", got, cap)
	}
}

// truncateReference is the []rune implementation Truncate used to be.
// It is not efficient, and it is not going back — it is here so the
// cheap one can be checked for byte-identical output, which is the only
// reason to prefer it.
func truncateReference(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}

// TestTruncate_MatchesTheRuneSliceByteForByte: the fix replaced
// []rune(s) with a rune-by-rune build because []rune allocates four
// bytes per byte for the whole input. Ranging over a string yields
// utf8.RuneError for an invalid byte while []rune yields the same rune,
// and WriteRune encodes it identically — so the outputs must match
// exactly, invalid input included.
//
// This is the assertion that makes the cheap version safe. A plain
// s[:cut] would have been faster still and silently stopped
// normalising invalid bytes to the replacement character.
func TestTruncate_MatchesTheRuneSliceByteForByte(t *testing.T) {
	inputs := []string{
		"",
		"short",
		"héllo→世界 🚀 mixed script",
		"a\xffb",               // one invalid byte
		"\xff\xff\xff\xff\xff", // invalid throughout
		"valid prefix \xc3 truncated mid-sequence",
		"abc\xffdef",
		strings.Repeat("é", 1000),
	}
	for _, in := range inputs {
		for _, n := range []int{-1, 0, 1, 3, 8, 14, 100, 1000, 5000} {
			got, want := Truncate(in, n), truncateReference(in, n)
			if got != want {
				t.Errorf("Truncate(%q, %d) = %q, []rune version = %q", in, n, got, want)
			}
		}
	}
}

// TestTruncate_DoesNotAmplifyLargeInput: six call sites pass a whole
// HTTP response body — bounded at 16 MB — into a 200- or 300-character
// truncation, all on error paths. []rune made that 64 MB of garbage to
// produce a 203-byte string.
func TestTruncate_DoesNotAmplifyLargeInput(t *testing.T) {
	body := strings.Repeat("x", 16<<20)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out := Truncate(body, 200)
	runtime.ReadMemStats(&after)

	if got, want := len(out), 203; got != want {
		t.Fatalf("output %d bytes, want %d — the cap itself is broken", got, want)
	}
	allocated := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(body))
	t.Logf("input 16.0 MB -> %d bytes out, transient %.3fx", len(out), allocated)
	if allocated > 0.01 {
		t.Errorf("truncating to 200 chars allocated %.3fx its input; the "+
			"whole string must not be materialised", allocated)
	}
}
