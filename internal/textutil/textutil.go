// Package textutil holds the string helpers shared across summarizer's
// packages. One home so a fix (rune-safety, cap semantics) lands once
// instead of once per copy.
package textutil

import (
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

// Truncate cuts s to at most n RUNES and appends an ellipsis marker
// when it cut anything. Rune-based, not byte-based: byte slicing splits
// multi-byte characters and leaves an invalid UTF-8 lead byte in the
// output, which then travels into an LLM prompt. Titles and descriptions
// in a Nordic or mixed-language repo hit this at the cap boundary.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}

// RetryAfter parses an HTTP Retry-After header, which may be
// delta-seconds ("120") or an HTTP-date, and returns the wait it asks
// for, capped at maxWait. A missing or unparseable header returns
// fallback so the caller keeps its own exponential backoff.
//
// One home for both callers: internal/gitlab and internal/llm each had
// their own copy, they drifted (min vs if), and they carried the same
// bug — which is why this lives with Truncate rather than beside it.
//
// The cap is applied to the SECONDS before the multiply, not to the
// Duration after it. time.Duration is int64 nanoseconds, so
// time.Duration(n) * time.Second overflows negative once n passes
// about 9.2e9 — and a negative duration means "no wait at all", which
// is the exact opposite of what a Retry-After asks for. Both copies
// multiplied first and capped after, so "9999999999" produced
// -2346317h and four immediate retries against an upstream that had
// asked for a pause.
func RetryAfter(h string, maxWait, fallback time.Duration) time.Duration {
	if h == "" {
		return fallback
	}
	var d time.Duration
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		if n > int(maxWait/time.Second) {
			return maxWait
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = max(time.Until(t), 0)
	} else {
		return fallback
	}
	// d cannot be negative here: the seconds branch is capped before
	// the multiply and the date branch is clamped above.
	return min(d, maxWait)
}
