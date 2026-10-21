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

// Fake is a test-only Guard so pipeline tests can drive both verdicts
// without an HTTP roundtrip. It lives in the test file on purpose: the
// production package has no business shipping a guard that answers
// whatever the caller asked for.
type Fake struct {
	Safe    bool
	Score   float64
	NextErr error
}

func (f *Fake) Check(_ context.Context, _ string) (Verdict, float64, error) {
	if f.NextErr != nil {
		return "", 0, f.NextErr
	}
	if f.Safe {
		return VerdictSafe, f.Score, nil
	}
	return VerdictInjection, f.Score, nil
}

// TestFakeGuard_SafeAndInjury: covers both verdicts plus the transport-error
// path (which the pipeline soft-fails on).
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
			var g Guard = tc.fake // Fake must satisfy the port
			v, _, err := g.Check(context.Background(), "anything")
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
