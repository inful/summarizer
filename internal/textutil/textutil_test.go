package textutil

import (
	"strings"
	"testing"
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
