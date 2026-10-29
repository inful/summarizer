package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
)

// makeEvent builds a small event for rendering tests. Updates is keyed
// off Created so formatting can use the standard event metadata.
func makeEvent(kind events.Kind, ref, title, author string, updated time.Time) events.NormalizedEvent {
	return events.NormalizedEvent{
		Kind: kind, Ref: ref, Title: title, Author: author,
		Updated: updated, Created: updated,
	}
}

// epicRef returns a pointer-to-Epic for inclusion in events.
func epicRef(iid int, title string) *gitlab.Epic {
	return &gitlab.Epic{IID: iid, Title: title, WebURL: "u/" + title}
}

// TestRender_EpicGrouped: when active epics are present, the report
// emits a "## Epics" section with one H3 per epic, the epic's linked
// issues nested underneath, and an "## Other activity" section for
// events without an epic link. The customer's browser would see a
// sidebar-of-progress instead of a flat list.
func TestRender_EpicGrouped(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	inWindow := now.Add(-48 * time.Hour)
	epicA := epicRef(1, "Deploy v2")
	epicB := epicRef(2, "Customer login refresh")

	evs := []events.NormalizedEvent{
		{Kind: events.KindIssue, Ref: "#11", Title: "Wire up auth", Updated: inWindow, Epic: epicA},
		{Kind: events.KindIssue, Ref: "#12", Title: "Roll out token refresh", Updated: inWindow, Epic: epicA},
		{Kind: events.KindIssue, Ref: "#21", Title: "Replace login form", Updated: inWindow, Epic: epicB},
		{Kind: events.KindMR, Ref: "#99", Title: "fix: typo in middleware", Author: "alice", Updated: inWindow},
		{Kind: events.KindCommit, Ref: "abc1234", Title: "kick off", Author: "bob", Created: inWindow},
	}
	epics := []*gitlab.Epic{epicA, epicB}

	_, user, err := Render(AudienceCustomer, "group/proj", evs, 200, epics, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Epic section present.
	if !strings.Contains(user, "## Epics active this period") {
		t.Errorf("missing epic section heading in:\n%s", user)
	}
	// Each epic as an H3 header.
	for _, e := range []*gitlab.Epic{epicA, epicB} {
		if !strings.Contains(user, "### "+e.Title) {
			t.Errorf("missing H3 for epic %q in:\n%s", e.Title, user)
		}
	}
	// Each issue appears nested under its epic, not flat.
	if !strings.Contains(user, "#11") || !strings.Contains(user, "#12") || !strings.Contains(user, "#21") {
		t.Errorf("expected issue refs in epic section; got:\n%s", user)
	}
	// Other-activity section present for the orphaned MR and commit.
	if !strings.Contains(user, "## Other activity") {
		t.Errorf("missing other-activity section in:\n%s", user)
	}
	if !strings.Contains(user, "#99") || !strings.Contains(user, "abc1234") {
		t.Errorf("orphaned events not in other-activity; got:\n%s", user)
	}
	// And the orphaned events are NOT in the epics section (would be
	// a layout bug worth catching).
	if strings.Contains(user, "### Deploy v2\n\n- #99") {
		t.Errorf("orphan MR leaked into epic A section")
	}
}

// TestRender_FallsBackToFlatWhenNoEpics: if no events have an Epic
// field populated, the report emits the existing flat structure with
// no Epic section. Backwards-compatible.
func TestRender_FallsBackToFlatWhenNoEpics(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	evs := []events.NormalizedEvent{
		makeEvent(events.KindMR, "#1", "first PR", "alice", now.Add(-24*time.Hour)),
		makeEvent(events.KindIssue, "#2", "weird bug", "bob", now.Add(-12*time.Hour)),
	}
	_, user, err := Render(AudienceCustomer, "group/proj", evs, 200, nil, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(user, "## Epics") {
		t.Errorf("no-epics input should not render an Epics section; got:\n%s", user)
	}
	if !strings.Contains(user, "#1") || !strings.Contains(user, "#2") {
		t.Errorf("flat events should still render; got:\n%s", user)
	}
}

// TestRender_EmptyEpicsSliceAlsoFlat: an explicit empty epics slice
// behaves the same as nil — backwards-compatible.
func TestRender_EmptyEpicsSliceAlsoFlat(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	evs := []events.NormalizedEvent{
		makeEvent(events.KindMR, "#1", "first PR", "alice", now),
	}
	_, user, err := Render(AudienceCustomer, "group/proj", evs, 200, []*gitlab.Epic{}, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(user, "## Epics") {
		t.Errorf("empty epics slice should render flat; got:\n%s", user)
	}
}

// TestRender_OnlyOrphansBecomesOtherActivity: when epics exist but no
// event in the period belongs to them, the report is purely the "other
// activity" section. (Edge case: caller passed epics but no event has
// Epic — happens when the only MRs/commits in the period don't close
// any fetched issue.)
func TestRender_OnlyOrphansBecomesOtherActivity(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	evs := []events.NormalizedEvent{
		makeEvent(events.KindMR, "#1", "first PR", "alice", now),
	}
	epics := []*gitlab.Epic{epicRef(99, "Inert epic")}
	_, user, err := Render(AudienceCustomer, "group/proj", evs, 200, epics, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(user, "## Other activity") {
		t.Errorf("orphans-only output should render the activity section")
	}
	if strings.Contains(user, "### Inert epic") {
		t.Errorf("inert epic should not surface when no event matches")
	}
}

// TestRenderEpicRollup_IsBounded: the two sibling renderers cap their
// input at DefaultRollupTotal. This one did not, and its ceiling is
// structural — MaxProjects × maxActiveEpics — so a full instance
// rendered 1,000 epic blocks at roughly 8× the combined rollup's
// budget, on the customer path, every week.
func TestRenderEpicRollup_IsBounded(t *testing.T) {
	var entries []EpicRollupEntry
	for p := 0; p < 50; p++ { // MaxProjects
		for e := 0; e < 20; e++ { // maxActiveEpics
			entries = append(entries, EpicRollupEntry{
				Project: fmt.Sprintf("group/p%02d", p),
				Epics: []EpicSummary{{
					Epic:       &gitlab.Epic{IID: e, Title: strings.Repeat("objective text ", 12), WebURL: "https://gitlab.example/g/p/-/epics/1"},
					EventCount: 7,
				}},
			})
		}
	}
	_, user, err := RenderEpicRollup(AudienceCustomer, entries, Limits{})
	if err != nil {
		t.Fatalf("RenderEpicRollup: %v", err)
	}
	if n := len(user); n > DefaultRollupTotal*2 {
		t.Errorf("prompt is %d bytes for %d entries, over the %d budget; "+
			"the epic renderer is the only one without a cap", n, len(entries), DefaultRollupTotal)
	}
	// Dropped from the FRONT, which Pipeline.Run orders
	// least-active-first — so the quiet projects are the ones that go.
	if strings.Contains(user, "p00") {
		t.Error("the first entry should have been dropped, not the last")
	}
	if !strings.Contains(user, "p49") {
		t.Error("the last entry should be kept")
	}
}
