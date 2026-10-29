package prompt

import (
	"strings"
	"testing"

	"github.com/inful/summarizer/internal/gitlab"
)

// TestRenderEpicRollup_Empty: when no project has active epics,
// render an empty-state message and still produce a (system, user)
// pair. Caller (the pipeline) is responsible for not publishing
// the empty file — RenderEpicRollup doesn't gate on count.
func TestRenderEpicRollup_Empty(t *testing.T) {
	_, user, err := RenderEpicRollup(AudienceCustomer, nil, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(user, "No active epics") {
		t.Errorf("empty epic rollup should mention 'No active epics', got:\n%s", user)
	}
}

// TestRenderEpicRollup_SingleProjectOneEpic: simplest non-empty case.
// A single project with one active epic surfaces in the rendered
// output with title, state, web URL, and event count.
func TestRenderEpicRollup_SingleProjectOneEpic(t *testing.T) {
	epic := &gitlab.Epic{
		IID: 7, Title: "Deploy v2", State: "opened",
		WebURL: "https://gitlab.example.com/groups/infra/-/epics/7",
	}
	entries := []EpicRollupEntry{
		{Project: "group/infra", Epics: []EpicSummary{
			{Epic: epic, EventCount: 3},
		}},
	}
	_, user, err := RenderEpicRollup(AudienceCustomer, entries, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"group/infra",
		"Deploy v2",
		"#7",
		"opened",
		"https://gitlab.example.com/groups/infra/-/epics/7",
		"3 events",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("rollup should contain %q, got:\n%s", want, user)
		}
	}
}

// TestRenderEpicRollup_MultipleProjectsEachWithEpics: the cross-
// project shape the operator wants. Two projects, each with their
// own active epics; one epic appears in BOTH projects' renderings
// (hypothetically — different projects could share an epic only
// across a GitLab group boundary, which the rollup makes visible).
func TestRenderEpicRollup_MultipleProjectsEachWithEpics(t *testing.T) {
	included := []EpicRollupEntry{
		{
			Project: "group/payments",
			Epics: []EpicSummary{
				{Epic: &gitlab.Epic{IID: 1, Title: "Auth refresh", State: "opened",
					WebURL: "u/1"}, EventCount: 12},
				{Epic: &gitlab.Epic{IID: 2, Title: "SLO enforcement", State: "closed",
					WebURL: "u/2"}, EventCount: 4},
			},
		},
		{
			Project: "group/billing",
			Epics: []EpicSummary{
				{Epic: &gitlab.Epic{IID: 3, Title: "Stripe webhooks", State: "opened",
					WebURL: "u/3"}, EventCount: 7},
			},
		},
	}
	_, user, err := RenderEpicRollup(AudienceCustomer, included, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Both project names should appear (cross-project structure).
	for _, proj := range []string{"group/payments", "group/billing"} {
		if !strings.Contains(user, proj) {
			t.Errorf("rollup should contain project %q, got:\n%s", proj, user)
		}
	}
	// All three epic titles should appear.
	for _, title := range []string{"Auth refresh", "SLO enforcement", "Stripe webhooks"} {
		if !strings.Contains(user, title) {
			t.Errorf("rollup should contain epic %q, got:\n%s", title, user)
		}
	}
}

// TestRenderEpicRollup_DifferentAudiencesProduceDifferentOutput:
// customer template uses "topic/area/feature" wording; team template
// uses "epic" and engineering-specific vocabulary. Sanity check the
// templates diverge.
func TestRenderEpicRollup_DifferentAudiencesProduceDifferentOutput(t *testing.T) {
	entries := []EpicRollupEntry{
		{Project: "g/p", Epics: []EpicSummary{
			{Epic: &gitlab.Epic{IID: 1, Title: "Auth", State: "opened"}, EventCount: 1},
		}},
	}
	_, custUser, err := RenderEpicRollup(AudienceCustomer, entries, Limits{})
	if err != nil {
		t.Fatalf("render customer: %v", err)
	}
	_, teamUser, err := RenderEpicRollup(AudienceTeam, entries, Limits{})
	if err != nil {
		t.Fatalf("render team: %v", err)
	}
	if custUser == teamUser {
		t.Errorf("customer and team rollups should differ; both:\n%s", custUser)
	}
}

// TestRenderEpicRollup_DisclosesProjectsDroppedByTheCap: when the
// 16 KB budget bites, whole project entries are dropped. The prompt
// must still name them, or the model synthesises a fleet-wide view from
// a subset and presents it as complete — on a file published
// public: true. RenderRollup already builds its project list before
// capping for exactly this reason.
func TestRenderEpicRollup_DisclosesProjectsDroppedByTheCap(t *testing.T) {
	// DefaultRollupTotal is 16000 and capEpicEntries sizes an entry at
	// ~(project + 72 + title + URL) per epic. Twenty epics of a long
	// title is ~9 KB per entry, so three entries overshoot the budget
	// twice over and the cap drops two of them.
	big := func(project string) EpicRollupEntry {
		epics := make([]EpicSummary, 0, 20)
		for i := 0; i < 20; i++ {
			epics = append(epics, EpicSummary{
				Epic:       &gitlab.Epic{IID: i, Title: strings.Repeat("objective ", 30), WebURL: "https://gitlab.example.com/g/p/-/epics/1"},
				EventCount: i,
			})
		}
		return EpicRollupEntry{Project: project, Epics: epics}
	}
	entries := []EpicRollupEntry{big("g/quiet"), big("g/also-quiet"), big("g/busy")}

	// The budget must bite, or this test proves nothing.
	kept := len(capEpicEntries(entries, DefaultRollupTotal))
	if kept == len(entries) {
		t.Fatalf("the budget did not bite (%d of %d entries kept)", kept, len(entries))
	}

	_, user, err := RenderEpicRollup(AudienceCustomer, entries, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The header counts every project, capped or not, and says how
	// many were left out. Naming each dropped project instead would
	// make the name list uncapped — 1,000 projects is ~30 KB against
	// a 16 KB budget — so the disclosure has to be a count.
	if !strings.Contains(user, "Projects in this epic rollup: 3") {
		t.Errorf("the header must count all 3 projects, not the kept ones:\n%s", firstLines(user, 6))
	}
	if !strings.Contains(user, "2 omitted for size") {
		t.Errorf("the prompt must disclose that 2 projects were dropped, so the "+
			"model cannot present a subset as the whole fleet:\n%s", firstLines(user, 6))
	}
	// And the prompt must still respect the budget it advertises.
	if len(user) > DefaultRollupTotal {
		t.Errorf("prompt is %d bytes for 3 entries; the cap is %d", len(user), DefaultRollupTotal)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
