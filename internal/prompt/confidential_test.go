package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
)

// The customer report is published with public: true. GitLab's
// "confidential" flag is the owner's own "not for everyone", and a
// read_api PAT owned by a project member receives those issues from
// /projects/:id/issues like any other.
//
// Executed before this was guarded:
//
//	customer  confidential EPIC title in prompt: true
//	customer  confidential ISSUE title in prompt: true
//
// So an issue titled "ACME pricing renegotiation — INTERNAL ONLY" was
// summarised into a file anyone could read. The repo already guarded the
// customer epic ROLLUP for exactly this — "it cannot go here whatever its
// labels say", at bucketEpicsByCustomer — but Render is the other path
// that renders epics, and it had no guard at all. The cross-project
// rollup consumes the rendered text, so it inherited the leak too.
func TestRender_ConfidentialNeverReachesTheCustomerPrompt(t *testing.T) {
	const (
		secretEpic  = "ACME pricing renegotiation — INTERNAL ONLY"
		secretIssue = "Secret: layoff plan Q4 — do not share"
	)

	confEpic := &gitlab.Epic{
		IID: 7, Title: secretEpic, Confidential: true,
		Labels: []string{"org::ACME"},
	}
	// An ordinary issue inside a confidential epic. Not itself
	// confidential, so filtering on the event's own flag alone would
	// still print the epic title and group this under it.
	childOfSecret := events.NormalizedEvent{
		Kind: events.KindIssue, Action: "opened", Ref: "#413",
		Title: "ordinary work under the secret epic", Updated: time.Now(),
		Epic: confEpic,
	}
	secretIssueEv := events.NormalizedEvent{
		Kind: events.KindIssue, Action: "opened", Ref: "#412",
		Title: secretIssue, Confidential: true, Updated: time.Now(),
		Labels: []string{"org::ACME"},
	}
	ordinary := events.NormalizedEvent{
		Kind: events.KindIssue, Action: "opened", Ref: "#500",
		Title: "shipped the billing exporter", Updated: time.Now(),
		Labels: []string{"org::ACME"},
	}

	evs := []events.NormalizedEvent{childOfSecret, secretIssueEv, ordinary}
	epics := []*gitlab.Epic{confEpic}

	_, user, err := Render(AudienceCustomer, "g/p", evs, 200, epics, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("customer render: %v", err)
	}
	for _, leaked := range []string{secretEpic, secretIssue, "ordinary work under the secret epic"} {
		if strings.Contains(user, leaked) {
			t.Errorf("customer prompt contains %q — this document is published public: true", leaked)
		}
	}
	if !strings.Contains(user, "shipped the billing exporter") {
		t.Error("the filter dropped ordinary work as well — it is over-broad")
	}

	// The team document is internal and keeps everything, or the flag
	// would be hiding work from the people who set it.
	_, teamUser, err := Render(AudienceTeam, "g/p", evs, 200, epics, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("team render: %v", err)
	}
	for _, want := range []string{secretEpic, secretIssue, "ordinary work under the secret epic"} {
		if !strings.Contains(teamUser, want) {
			t.Errorf("team prompt is missing %q — confidential work must stay visible internally", want)
		}
	}
}

// A confidential event must not consume a slot in the per-audience
// budget and push an ordinary one out of the report.
func TestRender_ConfidentialDoesNotConsumeTheEventBudget(t *testing.T) {
	secret := events.NormalizedEvent{
		Kind: events.KindIssue, Action: "opened", Ref: "#1",
		Title: "secret", Confidential: true, Updated: time.Now(),
	}
	ordinary := events.NormalizedEvent{
		Kind: events.KindIssue, Action: "opened", Ref: "#2",
		Title: "shipped the exporter", Updated: time.Now(),
	}
	// Cap of 1. Filtered first, the one surviving event is the ordinary
	// one; unfiltered, the secret wins the slot.
	_, user, err := Render(AudienceCustomer, "g/p", []events.NormalizedEvent{secret, ordinary}, 1, nil, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(user, "secret") {
		t.Error("the confidential event took the only slot")
	}
	if !strings.Contains(user, "shipped the exporter") {
		t.Error("the ordinary event was displaced by a confidential one")
	}
}

// forAudience is the single guard, so it is pinned directly too: it must
// return both slices untouched for the team audience rather than copying
// them for nothing.
func TestForAudience(t *testing.T) {
	epic := &gitlab.Epic{IID: 1, Confidential: true}
	evs := []events.NormalizedEvent{
		{Kind: events.KindIssue, Title: "own flag", Confidential: true},
		{Kind: events.KindIssue, Title: "epic flag", Epic: epic},
		{Kind: events.KindIssue, Title: "ordinary"},
	}
	eps := []*gitlab.Epic{epic, {IID: 2}}

	gotEvs, gotEpics := forAudience(AudienceTeam, evs, eps)
	if len(gotEvs) != 3 || len(gotEpics) != 2 {
		t.Errorf("team audience altered the input: %d events, %d epics", len(gotEvs), len(gotEpics))
	}

	gotEvs, gotEpics = forAudience(AudienceCustomer, evs, eps)
	if len(gotEvs) != 1 || gotEvs[0].Title != "ordinary" {
		t.Errorf("customer events = %d, want only the ordinary one: %+v", len(gotEvs), gotEvs)
	}
	if len(gotEpics) != 1 || gotEpics[0].IID != 2 {
		t.Errorf("customer epics = %+v, want only the non-confidential one", gotEpics)
	}
	// The input must not be mutated: runOneProject hands the same slice
	// to the team render after the customer one.
	if len(evs) != 3 || len(eps) != 2 {
		t.Error("forAudience mutated its input — the team render would see a filtered slice")
	}
}
