package prompt

import (
	"strings"
	"testing"
)

func TestRenderRollup_EmbedsAllProjects(t *testing.T) {
	projects := []ProjectReport{
		{Name: "group/alpha", Body: "# alpha\nShipped login."},
		{Name: "group/beta", Body: "# beta\nFixed pagination."},
	}
	system, user, err := RenderRollup(AudienceTeam, projects)
	if err != nil {
		t.Fatalf("render rollup: %v", err)
	}
	if !strings.Contains(system, "Markdown only") {
		t.Errorf("system prompt missing audience signature: %q", system)
	}
	for _, p := range projects {
		if !strings.Contains(user, p.Name) {
			t.Errorf("rollup missing project name %q", p.Name)
		}
		if !strings.Contains(user, p.Body) {
			t.Errorf("rollup missing body for %q", p.Name)
		}
	}
	if !strings.Contains(user, "<data>") {
		t.Errorf("rollup should wrap project bodies in <data> markers: %s", user)
	}
}

func TestRenderRollup_DropsOverflowByTotal(t *testing.T) {
	// Per-body cap is 4000 runes; each body is 5000 chars (gets capped
	// to 4000 + truncation marker = ~4008 runes). rollupTotalCap is 16000.
	// With 5 projects, capped total = ~20040 > 16000 — oldest projects
	// are dropped until the total fits.
	mkBody := func(ch byte) string { return strings.Repeat(string(ch), 5000) }
	projects := []ProjectReport{
		{Name: "p1", Body: mkBody('A')},
		{Name: "p2", Body: mkBody('B')},
		{Name: "p3", Body: mkBody('C')},
		{Name: "p4", Body: mkBody('D')},
		{Name: "p5", Body: mkBody('E')},
	}
	_, user, err := RenderRollup(AudienceTeam, projects)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Each marker appears as a long run inside <data>. A 100-char run
	// is unique enough that incidental single chars elsewhere don't
	// false-positive this check.
	if strings.Contains(user, strings.Repeat("A", 100)) {
		t.Errorf("expected oldest project (p1, A-marker) to be dropped when total exceeds cap")
	}
	if strings.Contains(user, strings.Repeat("B", 100)) {
		t.Errorf("expected second-oldest (p2, B-marker) to be dropped")
	}
	if !strings.Contains(user, strings.Repeat("E", 100)) {
		t.Errorf("expected most recent project (p5, E-marker) to remain")
	}
}

func TestRenderRollup_CapsIndividualBody(t *testing.T) {
	// One body of 10 KB — should be truncated to rollupPerBodyCap=4000 runes.
	big := strings.Repeat("a", 10000)
	projects := []ProjectReport{{Name: "only", Body: big}}
	_, user, err := RenderRollup(AudienceCustomer, projects)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(user, "[truncated]") {
		t.Errorf("expected individual body to be truncated, got: %s", user)
	}
}
