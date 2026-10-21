package run

import (
	"context"
	"errors"
	"testing"

	"github.com/inful/summarizer/internal/safety"
)

// rejectingGuard always returns an injection verdict; countingGuard
// records what it was asked about.
type rejectingGuard struct{ calls int }

func (g *rejectingGuard) Check(context.Context, string) (safety.Verdict, float64, error) {
	g.calls++
	return safety.VerdictInjection, 0.99, nil
}

type erroringGuard struct{ calls int }

func (g *erroringGuard) Check(context.Context, string) (safety.Verdict, float64, error) {
	g.calls++
	return "", 0, errors.New("decision API down")
}

type countingGuard struct{ calls int }

func (g *countingGuard) Check(context.Context, string) (safety.Verdict, float64, error) {
	g.calls++
	return safety.VerdictSafe, 0.1, nil
}

// TestMonthlyRunner_AppliesInjectionGuard: `summarizer monthly` feeds
// last month's published text — itself LLM output derived from GitLab
// issue and MR bodies — straight back to the LLM. Without the guard,
// `safety.enabled: true` is a promise the monthly path silently breaks.
func TestMonthlyRunner_AppliesInjectionGuard(t *testing.T) {
	weeks, _ := weeksInMonth("2026-09")
	repo := &monthlyRepo{branches: prefixes(weeks...), files: weeklyFiles(weeks...)}
	r, llmStub := newMonthlyRunner(t, repo)

	g := &rejectingGuard{}
	r.Guard = g

	_, err := r.Run(context.Background(), "2026-09")
	if err == nil {
		t.Fatal("an injection verdict must stop the monthly run")
	}
	if !errors.Is(err, ErrInjection) {
		t.Errorf("error should wrap ErrInjection so exit=8, got: %v", err)
	}
	if g.calls == 0 {
		t.Error("the guard was never consulted")
	}
	if len(llmStub.prompts) != 0 {
		t.Errorf("no LLM call may follow a rejection, got %d", len(llmStub.prompts))
	}
	if len(repo.files) == 0 {
		t.Fatal("fixture broken")
	}
}

// TestMonthlyRunner_GuardErrorSoftFails: a flaky decision API must not
// block generation — the structural <data> defence is still in place.
// Same policy as the per-project and rollup paths.
func TestMonthlyRunner_GuardErrorSoftFails(t *testing.T) {
	weeks, _ := weeksInMonth("2026-09")
	repo := &monthlyRepo{branches: prefixes(weeks...), files: weeklyFiles(weeks...)}
	r, llmStub := newMonthlyRunner(t, repo)

	g := &erroringGuard{}
	r.Guard = g

	if _, err := r.Run(context.Background(), "2026-09"); err != nil {
		t.Fatalf("a guard error must soft-fail, got: %v", err)
	}
	if g.calls == 0 {
		t.Error("the guard was never consulted")
	}
	if len(llmStub.prompts) != 2 {
		t.Errorf("expected both audiences to still be generated, got %d calls", len(llmStub.prompts))
	}
}

// TestMonthlyRunner_NilGuardDoesNotPanic: a nil Guard is a wiring
// mistake; it must degrade to "disabled", not crash mid-run.
func TestMonthlyRunner_NilGuardDoesNotPanic(t *testing.T) {
	weeks, _ := weeksInMonth("2026-09")
	repo := &monthlyRepo{branches: prefixes(weeks...), files: weeklyFiles(weeks...)}
	r, _ := newMonthlyRunner(t, repo)
	r.Guard = nil

	if _, err := r.Run(context.Background(), "2026-09"); err != nil {
		t.Errorf("nil guard should behave as disabled, got: %v", err)
	}
}

// TestCheckPrompt_SharedByBothPaths: the free function is the single
// gate for per-project, rollup and monthly prompts. A verdict must
// carry the label so the operator can tell which call was rejected.
func TestCheckPrompt_SharedByBothPaths(t *testing.T) {
	g := &rejectingGuard{}
	err := checkPrompt(context.Background(), g, "monthly-rollup/2026-09/customer", "some prompt")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, ErrInjection) {
		t.Errorf("want ErrInjection, got %v", err)
	}

	c := &countingGuard{}
	if err := checkPrompt(context.Background(), c, "group/p/team", "prompt"); err != nil {
		t.Errorf("a safe verdict must pass through, got %v", err)
	}
	if c.calls != 1 {
		t.Errorf("guard consulted %d times, want 1", c.calls)
	}
}
