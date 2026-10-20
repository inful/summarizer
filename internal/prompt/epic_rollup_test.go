package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// TestRenderEpicRollup_Empty: when no project has active epics,
// render an empty-state message and still produce a (system, user)
// pair. Caller (the pipeline) is responsible for not publishing
// the empty file — RenderEpicRollup doesn't gate on count.
func TestRenderEpicRollup_Empty(t *testing.T) {
	_, user, err := RenderEpicRollup(AudienceCustomer, "group/x", 7*24*time.Hour, nil)
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
	_, user, err := RenderEpicRollup(AudienceCustomer, "group/infra", 7*24*time.Hour, entries)
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
	_, user, err := RenderEpicRollup(AudienceCustomer, "group/*", 7*24*time.Hour, included)
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
	_, custUser, err := RenderEpicRollup(AudienceCustomer, "g/p", 7*24*time.Hour, entries)
	if err != nil {
		t.Fatalf("render customer: %v", err)
	}
	_, teamUser, err := RenderEpicRollup(AudienceTeam, "g/p", 7*24*time.Hour, entries)
	if err != nil {
		t.Fatalf("render team: %v", err)
	}
	if custUser == teamUser {
		t.Errorf("customer and team rollups should differ; both:\n%s", custUser)
	}
}
