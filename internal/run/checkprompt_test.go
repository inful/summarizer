package run

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/inful/summarizer/internal/safety"
)

// captureStderr runs fn with os.Stderr redirected and returns what was
// written. The guard's log lines are the only way an operator can tell
// whether the injection guard actually ran, so they are worth a test.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestCheckPrompt_NoopGuardClaimsNothing: safety.enabled is opt-in, so
// the default install runs with a NoopGuard — which answers "safe" to a
// question it was never asked, with a score of exactly 0.000.
//
// The log used to say so out loud:
//
//	summarizer: safety guard cleared g/p/customer (score=0.000)
//
// which is a positive security claim about a check that did not happen.
// An operator reading a cron log — or an auditor — would conclude the
// prompts had been screened against injection.
func TestCheckPrompt_NoopGuardClaimsNothing(t *testing.T) {
	for name, g := range map[string]safety.Guard{
		"value":   safety.NoopGuard{},
		"pointer": &safety.NoopGuard{},
		"nil":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			out := captureStderr(t, func() {
				if err := checkPrompt(context.Background(), g, "g/p/customer", "prompt"); err != nil {
					t.Errorf("checkPrompt: %v", err)
				}
			})
			if strings.Contains(out, "cleared") {
				t.Errorf("a disabled guard reported a result:\n%s", out)
			}
			if strings.Contains(out, "safety guard") {
				t.Errorf("a disabled guard should say nothing at all:\n%s", out)
			}
		})
	}
}

// A guard that DID run must still report its result — that is the line
// an operator uses to see the control is live.
func TestCheckPrompt_RealGuardStillReports(t *testing.T) {
	g := &countingGuard{}
	out := captureStderr(t, func() {
		if err := checkPrompt(context.Background(), g, "g/p/customer", "prompt"); err != nil {
			t.Errorf("checkPrompt: %v", err)
		}
	})
	if g.calls != 1 {
		t.Errorf("the guard was called %d times, want 1", g.calls)
	}
	if !strings.Contains(out, "safety guard cleared g/p/customer (score=0.100)") {
		t.Errorf("a real guard's result was not reported:\n%s", out)
	}
}

// And a rejecting guard must still hard-fail, with its own line.
func TestCheckPrompt_RejectingGuardStillFails(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = checkPrompt(context.Background(), &rejectingGuard{}, "g/p/customer", "prompt")
	})
	if !errors.Is(err, ErrInjection) {
		t.Fatalf("err = %v, want ErrInjection", err)
	}
	if !strings.Contains(out, "safety guard REJECTED prompt for g/p/customer (score=0.990 >= threshold)") {
		t.Errorf("the rejection was not reported:\n%s", out)
	}
}
