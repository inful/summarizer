package safety

import (
	"context"
	"errors"
	"testing"
)

// TestNoopGuard_AlwaysSafe: the noop is the default-when-disabled path;
// it must never block, never error, and always return VerdictSafe.
func TestNoopGuard_AlwaysSafe(t *testing.T) {
	var g Guard = NoopGuard{}
	v, n, err := g.Check(context.Background(), "anything")
	if err != nil {
		t.Fatalf("noop should not error: %v", err)
	}
	if v != VerdictSafe {
		t.Errorf("noop verdict = %q, want safe", v)
	}
	if n != 0 {
		t.Errorf("noop score = %f, want 0", n)
	}
}

// TestFakeGuard_SafeAndInjury: covers both verdicts via the test-friendly
// Fake guard so pipeline tests can drive both paths without an HTTP roundtrip.
func TestFakeGuard_SafeAndInjury(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fake    *Fake
		wantVer Verdict
		wantErr bool
	}{
		{"safe", &Fake{Safe: true, Score: 0.05}, VerdictSafe, false},
		{"injection", &Fake{Safe: false, Score: 0.95}, VerdictInjection, false},
		{"fake-err", &Fake{NextErr: errors.New("boom")}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, err := tc.fake.Check(context.Background(), "anything")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if v != tc.wantVer {
				t.Errorf("verdict = %q, want %q", v, tc.wantVer)
			}
		})
	}
}
