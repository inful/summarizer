package events

import (
	"strconv"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// proj is the project the events in this file belong to. Every issue
// fixture carries a matching reference, because AttachEpics now
// correlates on (project, IID) rather than IID alone.
const proj = "group/app"

// issue builds an epic issue that belongs to proj.
func issue(iid int, title string) gitlab.Issue {
	return gitlab.Issue{
		IID: iid, Title: title,
		References: gitlab.IssueRef{Full: proj + "#" + strconv.Itoa(iid)},
	}
}

// TestAttachEpics_AnnotatesMatchingIssue: the issue-keyed epic map is
// the primary correlation path. An Issue event matching a project-local
// issue gets Epic populated.
func TestAttachEpics_AnnotatesMatchingIssue(t *testing.T) {
	epic := &gitlab.Epic{
		IID: 7, Title: "Deploy v2", WebURL: "u/7",
		Issues: []gitlab.Issue{
			issue(100, "Wire up auth"),
			issue(101, "Roll out token refresh"),
		},
	}

	events := []NormalizedEvent{
		{Kind: KindIssue, Ref: "#100", Title: "Wire up auth"},
		{Kind: KindIssue, Ref: "#102", Title: "Unrelated"},
	}

	AttachEpics(events, []*gitlab.Epic{epic}, proj)

	if events[0].Epic == nil {
		t.Fatal("matching issue event should have Epic populated")
	}
	if events[0].Epic.IID != 7 {
		t.Errorf("event got wrong epic: %+v", events[0].Epic)
	}
	if events[1].Epic != nil {
		t.Errorf("non-matching event should not be annotated")
	}
}

// TestAttachEpics_FirstMatchOnly: if the same IID somehow appears on
// multiple epics (operator error in GitLab), the first epic wins.
// Predictable behavior beats undefined behavior.
func TestAttachEpics_FirstMatchOnly(t *testing.T) {
	first := &gitlab.Epic{IID: 1, Title: "First", Issues: []gitlab.Issue{issue(5, "dup")}}
	second := &gitlab.Epic{IID: 2, Title: "Second", Issues: []gitlab.Issue{issue(5, "dup")}}

	events := []NormalizedEvent{{Kind: KindIssue, Ref: "#5"}}
	AttachEpics(events, []*gitlab.Epic{first, second}, proj)

	if events[0].Epic == nil || events[0].Epic.IID != 1 {
		t.Errorf("expected first epic to win on duplicate IID, got %+v", events[0].Epic)
	}
}

// TestAttachEpics_IssueRefParsing: the helper extracts issue IID from
// refs formatted "#123" or "123" — both are valid for our purposes.
// The "#" prefix is GitLab's display format used elsewhere.
func TestAttachEpics_IssueRefParsing(t *testing.T) {
	epic := &gitlab.Epic{
		IID: 99, Title: "Single",
		Issues: []gitlab.Issue{issue(42, "only")},
	}
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"hash prefix", "#42", true},
		{"bare number", "42", true},
		{"non-numeric", "abc", false},
		{"empty", "", false},
		{"negative", "-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []NormalizedEvent{{Kind: KindIssue, Ref: tc.ref}}
			AttachEpics(events, []*gitlab.Epic{epic}, proj)
			got := events[0].Epic != nil
			if got != tc.want {
				t.Errorf("ref=%q Epic attached = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestAttachEpics_CommitsNotAnnotated: a Commit event has no issue
// link in our model (commits don't carry issue references in this
// code path). AttachEpics should leave them alone even if the ref
// happens to look like an issue number.
func TestAttachEpics_CommitsNotAnnotated(t *testing.T) {
	epic := &gitlab.Epic{
		IID: 7, Title: "Deploy v2",
		Issues: []gitlab.Issue{issue(100, "commit-ish")},
	}
	events := []NormalizedEvent{{
		Kind: KindCommit, Ref: "100", Title: "fix typo",
		Author: "bob", Created: time.Now(),
	}}
	AttachEpics(events, []*gitlab.Epic{epic}, proj)
	if events[0].Epic != nil {
		t.Errorf("commit event should never be annotated with an epic, got %+v", events[0].Epic)
	}
}

// TestFilterActiveEpics: only epics with at least one of their issues
// having an updated_at within [since, until] count as "active". The
// 20-cap applies to active epics, not the whole epic list — a project
// with 50 epics and only 5 active this period surfaces all 5, even
// though the inactive ones never appear in the report.
func TestFilterActiveEpics(t *testing.T) {
	since := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	inWindow := since.Add(48 * time.Hour)
	before := since.Add(-48 * time.Hour)
	after := until.Add(48 * time.Hour)

	epics := []*gitlab.Epic{
		{IID: 1, Title: "Active now", Issues: []gitlab.Issue{
			{UpdatedAt: inWindow}, {UpdatedAt: before},
		}},
		{IID: 2, Title: "Quiet, dropped", Issues: []gitlab.Issue{
			{UpdatedAt: before}, {UpdatedAt: before},
		}},
		{IID: 3, Title: "Future", Issues: []gitlab.Issue{
			{UpdatedAt: after},
		}},
		{IID: 4, Title: "Edge of window", Issues: []gitlab.Issue{
			{UpdatedAt: until}, // exactly on the boundary counts as active
		}},
	}

	got := FilterActiveEpics(epics, since, until)
	want := []int{1, 4}
	if len(got) != len(want) {
		t.Fatalf("active epics: want %d, got %d (%+v)", len(want), len(got), got)
	}
	for i, e := range got {
		if e.IID != want[i] {
			t.Errorf("active epics[%d] IID = %d, want %d", i, e.IID, want[i])
		}
	}
}

// TestFilterActiveEpics_NoIssuesIsInactive: an epic with zero issues
// is, by definition, inactive this period — even if the epic itself
// has a recent update_at. (Issue activity is what we care about;
// the epic header without issues is inert and not worth a section.)
func TestFilterActiveEpics_NoIssuesIsInactive(t *testing.T) {
	since := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	epics := []*gitlab.Epic{
		{IID: 1, Title: "Empty epic", UpdatedAt: until, Issues: []gitlab.Issue{}},
	}
	got := FilterActiveEpics(epics, since, until)
	if len(got) != 0 {
		t.Errorf("empty epic should not be active, got %+v", got)
	}
}

// TestAttachEpics_DoesNotConfuseProjects: GitLab issue IIDs are unique
// per PROJECT, and an epic's issues come back group-wide. So "#10" in
// project A and "#10" in project B are different issues, and keying the
// lookup on the IID alone filed A's work under B's epic.
//
// The consequences were not cosmetic: the epic's per-project event
// count came out wrong, and policy.EpicCustomerCodes reads the wrong
// epic's org:: labels — so ACME's activity could be counted and billed
// under GLOBEX, in a rollup published public: true.
func TestAttachEpics_DoesNotConfuseProjects(t *testing.T) {
	platform := &gitlab.Epic{IID: 1, Title: "Platform", Issues: []gitlab.Issue{
		{IID: 10, Title: "A's login breaks", References: gitlab.IssueRef{Full: "g/a#10"}},
	}}
	billing := &gitlab.Epic{IID: 2, Title: "Billing", Issues: []gitlab.Issue{
		{IID: 10, Title: "B's invoice fails", References: gitlab.IssueRef{Full: "g/b#10"}},
	}}

	// Billing comes first, so first-wins used to pick it for A.
	evs := []NormalizedEvent{{Kind: KindIssue, Ref: "#10", Title: "A's login breaks"}}
	AttachEpics(evs, []*gitlab.Epic{billing, platform}, "g/a")
	if evs[0].Epic == nil || evs[0].Epic.Title != "Platform" {
		t.Fatalf("project a's issue #10 got epic %+v, want Platform", evs[0].Epic)
	}

	// And the other way round: B's own issue still resolves to Billing,
	// so the filter is not just refusing every non-first epic.
	evsB := []NormalizedEvent{{Kind: KindIssue, Ref: "#10", Title: "B's invoice fails"}}
	AttachEpics(evsB, []*gitlab.Epic{billing, platform}, "g/b")
	if evsB[0].Epic == nil || evsB[0].Epic.Title != "Billing" {
		t.Fatalf("project b's issue #10 got epic %+v, want Billing", evsB[0].Epic)
	}
}

// TestAttachEpics_SkipsIssuesWithNoProjectReference: an issue GitLab
// sent no reference for cannot be placed in this project. Skipping it
// costs a grouping; guessing costs a customer their own work.
func TestAttachEpics_SkipsIssuesWithNoProjectReference(t *testing.T) {
	epic := &gitlab.Epic{IID: 1, Title: "Deploy v2", Issues: []gitlab.Issue{{IID: 7}}}
	evs := []NormalizedEvent{{Kind: KindIssue, Ref: "#7"}}
	AttachEpics(evs, []*gitlab.Epic{epic}, proj)
	if evs[0].Epic != nil {
		t.Errorf("an issue with no project reference must not be correlated: %+v", evs[0].Epic)
	}
}

// TestAttachEpics_ProjectMatchIsCaseInsensitive: GitLab project routes
// are case-insensitive, so `--project Group/Sub/App` reaches the same
// project as the canonical `group/sub/app` and works for MRs, issues,
// commits and discovery. An exact compare made the epic section the
// one place the operator's spelling mattered — and it failed silently,
// with every epic gone from every rollup.
func TestAttachEpics_ProjectMatchIsCaseInsensitive(t *testing.T) {
	epic := &gitlab.Epic{IID: 1, Title: "Platform", Issues: []gitlab.Issue{
		{IID: 10, Title: "login breaks", References: gitlab.IssueRef{Full: "group/sub/App#10"}},
	}}
	for _, typed := range []string{"group/sub/App", "group/sub/app", "Group/Sub/App", "GROUP/SUB/APP"} {
		evs := []NormalizedEvent{{Kind: KindIssue, Ref: "#10"}}
		AttachEpics(evs, []*gitlab.Epic{epic}, typed)
		if evs[0].Epic == nil {
			t.Errorf("--project %q lost the epic; GitLab routes are case-insensitive", typed)
		}
	}
	// And a genuinely different project still does not match, case
	// aside — the filter is not just returning true for everything.
	evs := []NormalizedEvent{{Kind: KindIssue, Ref: "#10"}}
	AttachEpics(evs, []*gitlab.Epic{epic}, "group/other/app")
	if evs[0].Epic != nil {
		t.Error("an issue from another project must not be correlated")
	}
}
