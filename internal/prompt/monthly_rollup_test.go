package prompt

import (
	"strings"
	"testing"
)

// TestRenderMonthlyRollup_LabelsWeeksNotProjects: the one thing that
// separates the monthly prompt from the weekly rollup prompt. If this
// ever said "Project: 2026-W36", the model would write a week-by-week
// project digest — exactly the output the monthly format exists to
// avoid — while still looking correct at a glance.
func TestRenderMonthlyRollup_LabelsWeeksNotProjects(t *testing.T) {
	weeks := []ProjectReport{
		{Name: "2026-W37", Body: "shipped the importer"},
		{Name: "2026-W38", Body: "fixed the importer's timezone bug"},
		{Name: "2026-W39", Body: "importer rolled out to two customers"},
	}
	system, user, err := RenderMonthlyRollup(AudienceCustomer, "2026-09", weeks, Limits{})
	if err != nil {
		t.Fatalf("RenderMonthlyRollup: %v", err)
	}
	for _, w := range []string{"2026-W37", "2026-W38", "2026-W39"} {
		if !strings.Contains(user, "# Week of "+w) {
			t.Errorf("missing week header for %s", w)
		}
	}
	if strings.Contains(user, "# Project:") {
		t.Error("monthly prompt must label weeks, not projects")
	}
	if !strings.Contains(user, "2026-09") {
		t.Error("month missing from the prompt")
	}
	// The prompt must name the weeks it covers. Asserting the presence
	// of the header rather than the absence of the old one: "not
	// Window:" passes just as happily on a prompt with no header at
	// all, which is the case this is here to catch.
	for _, w := range []string{"2026-W37", "2026-W38", "2026-W39"} {
		if !strings.Contains(user, "  - "+w+"\n") {
			t.Errorf("monthly prompt must list %s under the week manifest:\n%s", w, user)
		}
	}
	if !strings.Contains(user, "Weeks in this rollup: 3") {
		t.Error("monthly prompt must state how many weeks it covers")
	}
	if system == "" {
		t.Error("system prompt must not be empty")
	}
}

// TestRenderMonthlyRollup_KeepsEveryWeekUnderCap: a normal month is 4-5
// weeks and should never trip the overflow path.
func TestRenderMonthlyRollup_KeepsEveryWeekUnderCap(t *testing.T) {
	var weeks []ProjectReport
	for i := 37; i <= 40; i++ {
		weeks = append(weeks, ProjectReport{
			Name: "2026-W" + string(rune('0'+i%10)) + "x",
			Body: strings.Repeat("body ", 100),
		})
	}
	_, user, err := RenderMonthlyRollup(AudienceTeam, "2026-09", weeks, Limits{})
	if err != nil {
		t.Fatalf("RenderMonthlyRollup: %v", err)
	}
	for _, w := range weeks {
		if !strings.Contains(user, w.Name) {
			t.Errorf("week %s was dropped but should have fit under the cap", w.Name)
		}
	}
}

// TestRenderMonthlyRollup_EmptyStillRenders: a month with no weeks
// produces a prompt rather than panicking. The runner refuses to call
// it in that state, but the renderer should not explode if it does.
func TestRenderMonthlyRollup_EmptyStillRenders(t *testing.T) {
	if _, _, err := RenderMonthlyRollup(AudienceCustomer, "2026-09", nil, Limits{}); err != nil {
		t.Errorf("empty input should still render: %v", err)
	}
}

// TestRenderMonthlyRollup_AudienceTemplatesDiffer: the two audiences
// must not share a template — the customer one strips GitLab
// vocabulary, the team one keeps it.
func TestRenderMonthlyRollup_AudienceTemplatesDiffer(t *testing.T) {
	weeks := []ProjectReport{{Name: "2026-W37", Body: "x"}}
	_, cust, err := RenderMonthlyRollup(AudienceCustomer, "2026-09", weeks, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	_, team, err := RenderMonthlyRollup(AudienceTeam, "2026-09", weeks, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if cust == team {
		t.Error("customer and team monthly prompts must differ")
	}
	if !strings.Contains(cust, "people who use the software") {
		t.Error("customer prompt lost its audience framing")
	}
	if !strings.Contains(team, "internal engineering") {
		t.Error("team prompt lost its audience framing")
	}
}
