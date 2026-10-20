package gitlab

import (
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