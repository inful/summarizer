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

// TestCapBodies_DropsFromTheFrontAndKeepsTheNameList: the disclosure
// contract both rollups share. `names` is the full input and keeps its
// order, so the template still names every project; `bodies` loses its
// front entries until the total fits. At least one body survives.
func TestCapBodies_DropsFromTheFrontAndKeepsTheNameList(t *testing.T) {
	// Five entries at the per-body cap is 20 KB against a 16 KB budget,
	// so exactly one is dropped. Four would land on the cap exactly and
	// keep everything.
	names := []string{"a", "b", "c", "d", "e"}
	bodies := map[string]string{
		"a": string(make([]byte, rollupPerBodyCap)),
		"b": string(make([]byte, rollupPerBodyCap)),
		"c": string(make([]byte, rollupPerBodyCap)),
		"d": string(make([]byte, rollupPerBodyCap)),
		"e": string(make([]byte, rollupPerBodyCap)),
	}
	before := len(names)

	capBodies(names, bodies)

	if len(names) != before {
		t.Errorf("names shrank from %d to %d; the model must still see every "+
			"project so it can tell a subset from the whole", before, len(names))
	}
	if names[0] != "a" || names[len(names)-1] != "e" {
		t.Errorf("names reordered: %v", names)
	}
	if len(bodies) == 0 {
		t.Fatal("every body was dropped; the template would render nothing")
	}
	if len(bodies) >= len(names) {
		t.Errorf("nothing was dropped: %d of %d bodies kept under a %d cap",
			len(bodies), len(names), rollupTotalCap)
	}
	if _, ok := bodies["a"]; ok {
		t.Error("a is the front of the list and the first to go, but its body survived")
	}
	if _, ok := bodies["e"]; !ok {
		t.Error("e is last and must never be the one dropped")
	}
}

// TestCapBodies_UnderTheCapChangesNothing: the ordinary case.
func TestCapBodies_UnderTheCapChangesNothing(t *testing.T) {
	names := []string{"a", "b"}
	bodies := map[string]string{"a": "small", "b": "small"}
	capBodies(names, bodies)
	if len(bodies) != 2 {
		t.Errorf("bodies = %v, want both kept", bodies)
	}
}

// TestCapBodies_KeepsOneEvenWhenOneAloneExceedsTheCap: a single
// oversized body must not leave the template empty.
func TestCapBodies_KeepsOneEvenWhenOneAloneExceedsTheCap(t *testing.T) {
	names := []string{"only"}
	bodies := map[string]string{"only": string(make([]byte, rollupTotalCap*2))}
	capBodies(names, bodies)
	if len(bodies) != 1 {
		t.Errorf("bodies = %v, want the one entry kept", bodies)
	}
}
