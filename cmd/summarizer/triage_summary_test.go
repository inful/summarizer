package main

import (
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/run"
	"github.com/inful/summarizer/internal/triage"
)

// TestNewTriage_DisabledIsANoop: triage is opt-in. A config that never
// mentions it must get no classifier — and the pipeline keys off nil,
// so a zero-value Triage would silently classify every project.
func TestNewTriage_DisabledIsANoop(t *testing.T) {
	cfg := &config.Config{GitLabHost: "https://x"}
	cfg.Defaults()
	tr, code := newTriage(cfg)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if tr != nil {
		t.Errorf("triage = %v, want nil when the config does not enable it", tr)
	}
}

// TestNewTriage_NeedsADecisionURL: enabled with nowhere to send the
// classification must fail as a config error, not fall back to some
// other endpoint or silently classify nothing.
func TestNewTriage_NeedsADecisionURL(t *testing.T) {
	cfg := &config.Config{GitLabHost: "https://x", Triage: config.TriageConfig{Enabled: true}}
	cfg.Defaults()
	if _, code := newTriage(cfg); code != exitConfig {
		t.Errorf("code = %d, want %d — triage.enabled with no decision URL", code, exitConfig)
	}
}

// TestNewTriage_FallsBackToTheSafetyConnection: it is the same
// service, so an operator who already set safety.decision_url gets it
// without repeating themselves.
func TestNewTriage_FallsBackToTheSafetyConnection(t *testing.T) {
	cfg := &config.Config{
		GitLabHost: "https://x",
		Safety:     config.SafetyConfig{DecisionURL: "https://decide.example"},
		Triage:     config.TriageConfig{Enabled: true},
	}
	cfg.Defaults()
	tr, code := newTriage(cfg)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if tr == nil {
		t.Fatal("triage = nil, want a classifier")
	}
	if tr.Mode != triage.ModeWeek {
		t.Errorf("mode = %q, want the default %q", tr.Mode, triage.ModeWeek)
	}
}

// TestNewTriage_HonoursTheConfiguredMode: the mode is the whole
// cost decision, so a configured mode must survive the wiring.
func TestNewTriage_HonoursTheConfiguredMode(t *testing.T) {
	cfg := &config.Config{
		GitLabHost: "https://x",
		Triage:     config.TriageConfig{Enabled: true, DecisionURL: "https://d.example", Mode: triage.ModeMR},
	}
	cfg.Defaults()
	tr, code := newTriage(cfg)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if tr.Mode != triage.ModeMR {
		t.Errorf("mode = %q, want %q", tr.Mode, triage.ModeMR)
	}
}

// TestPrintSummary_ReportsTriageCost: the two triage call shapes differ
// in cost by two orders of magnitude, so an operator choosing between
// them needs the number. Without it, "triage is on" is not a decision
// anyone can make.
func TestPrintSummary_ReportsTriageCost(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{{
			Project: "a", EventsFetched: 10, EventsUsed: 10,
			FilesWritten: []string{"a.md"}, CustomerOn: true,
			Triage: triage.Result{Calls: 155, InputTokens: 57505, OutputTokens: 20835},
		}},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	for _, want := range []string{"triage[a]", "155 call(s)", "57505 in", "20835 out"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not report %q:\n%s", want, out)
		}
	}
}

// TestPrintSummary_SaysWhenTriageFailed: a report that lost its
// classifier hint reads exactly like a report that found no blockers.
// The summary has to say which one it was, or the operator cannot tell
// "nothing is blocked" from "nobody asked".
func TestPrintSummary_SaysWhenTriageFailed(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{{
			Project: "a", EventsFetched: 10, EventsUsed: 10,
			FilesWritten: []string{"a.md"}, CustomerOn: true,
			TriageFailed: true,
		}},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if !strings.Contains(out, "triage[a]") || !strings.Contains(out, "FAILED") {
		t.Errorf("the summary does not report the failed classification:\n%s", out)
	}
}

// TestPrintSummary_NoTriageLineWhenTriageIsOff: triage is off by
// default, and a summary line about a feature that did not run is
// noise on every run in the default configuration.
func TestPrintSummary_NoTriageLineWhenTriageIsOff(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{{
			Project: "a", EventsFetched: 10, EventsUsed: 10,
			FilesWritten: []string{"a.md"}, CustomerOn: true,
		}},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if strings.Contains(out, "triage[") {
		t.Errorf("the summary reports triage for a run that did none:\n%s", out)
	}
}
