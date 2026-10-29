package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
)

// TestRender_RespectsTheDiffBudget: the per-MR diff budget is a
// config key, so the prompt builder has to honour what it is given —
// not the constant it used to carry. A cap the operator raises and
// nothing reads is a lie in the config file.
func TestRender_RespectsTheDiffBudget(t *testing.T) {
	evs := []events.NormalizedEvent{{
		Kind: events.KindMR, Action: "merged", Ref: "#1",
		Title: "Big change", Updated: time.Now(),
		DiffChunks: []gitlab.DiffChunk{
			{Path: "a.go", Diff: strings.Repeat("Ω", 3000)},
			{Path: "b.go", Diff: strings.Repeat("Ω", 3000)},
		},
	}}

	_, small, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{}, Limits{DiffPerMR: 100})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, big, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{}, Limits{DiffPerMR: 5000})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Ω appears nowhere else in the prompt, so the count is the diff
	// body and nothing else. The truncation marker is annotation and
	// does not count against the budget.
	count := func(s string) int { return strings.Count(s, "Ω") }
	if n := count(small); n > 100 {
		t.Errorf("a 100-byte budget emitted %d bytes of diff", n)
	}
	if count(big) <= count(small) {
		t.Errorf("raising the budget did not keep more diff: %d vs %d", count(big), count(small))
	}
}

// TestRender_ZeroLimitsMeansTheDefaults: a caller that does not care
// about a budget passes a zero Limits. It must get the documented
// defaults, not an empty prompt.
func TestRender_ZeroLimitsMeansTheDefaults(t *testing.T) {
	evs := []events.NormalizedEvent{{
		Kind: events.KindMR, Action: "merged", Ref: "#1",
		Title: "Ship the exporter", Updated: time.Now(),
		DiffChunks: []gitlab.DiffChunk{
			{Path: "a.go", Diff: strings.Repeat("x", 3000)},
			{Path: "b.go", Diff: strings.Repeat("y", 3000)},
		},
	}}

	_, zero, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	_, defaults, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{}, DefaultLimits())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if zero != defaults {
		t.Error("a zero Limits did not render the same as DefaultLimits()")
	}
}

// TestRenderRollup_RespectsTheBodyBudget: the per-project body budget
// in a rollup prompt is a config key too.
func TestRenderRollup_RespectsTheBodyBudget(t *testing.T) {
	reports := []ProjectReport{{
		Name: "group/a",
		Body: strings.Repeat("Ω", 5000),
	}}

	_, small, err := RenderRollup(AudienceTeam, reports, Limits{RollupPerBody: 100})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if n := strings.Count(small, "Ω"); n > 100 {
		t.Errorf("a 100-rune body budget emitted %d runes", n)
	}
	if !strings.Contains(small, "...[truncated]") {
		t.Error("a truncated body must say so")
	}

	_, big, err := RenderRollup(AudienceTeam, reports, Limits{RollupPerBody: 4000})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Count(big, "Ω") <= strings.Count(small, "Ω") {
		t.Error("raising the body budget did not keep more of the report")
	}
}
