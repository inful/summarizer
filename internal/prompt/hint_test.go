package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
)

// TestRender_BlockedIsAskedOnlyWhenTheClassifierFoundSome: the team
// prompt asked for "what is blocked" unconditionally, and nothing in
// the pipeline collected anything that meant blocked. The model either
// invented blockers or wrote "nothing appears blocked", and neither is
// an answer.
//
// With a classifier hint, the prompt asks for blockers AND carries the
// note that there is something to find. Without one, it does not ask a
// question the data cannot answer.
func TestRender_BlockedIsAskedOnlyWhenTheClassifierFoundSome(t *testing.T) {
	evs := []events.NormalizedEvent{{
		Kind: events.KindMR, Action: "merged", Ref: "#1",
		Title: "Ship the exporter", Updated: time.Now(),
	}}

	_, without, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(without, "what is blocked") {
		t.Error("the prompt asks about blockers with nothing to find them in")
	}
	if strings.Contains(without, "classifier") {
		t.Errorf("a hint appeared with no hint:\n%s", without)
	}

	_, with, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{
		Blocked: true, BlockedScore: 0.83,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(with, "what is blocked") {
		t.Error("the prompt does not ask about blockers even though the classifier found some")
	}
	if !strings.Contains(with, "0.83") {
		t.Errorf("the classifier's score is not in the prompt:\n%s", with)
	}
}

// TestRender_ClassifierHintStaysOutOfTheCustomerPrompt: the blocker
// note is internal information — the customer document is published
// public: true, and "our team is blocked on X" is not a customer
// update.
func TestRender_ClassifierHintStaysOutOfTheCustomerPrompt(t *testing.T) {
	evs := []events.NormalizedEvent{{
		Kind: events.KindMR, Action: "merged", Ref: "#1",
		Title: "Ship the exporter", Updated: time.Now(),
	}}
	hint := Hint{Blocked: true, BlockedScore: 0.83, PeerRelevant: true, PeerScore: 0.7}

	_, team, err := Render(AudienceTeam, "g/p", evs, 200, nil, hint)
	if err != nil {
		t.Fatalf("team render: %v", err)
	}
	if !strings.Contains(team, "0.83") {
		t.Errorf("the team prompt lost the hint:\n%s", team)
	}

	_, customer, err := Render(AudienceCustomer, "g/p", evs, 200, nil, hint)
	if err != nil {
		t.Fatalf("customer render: %v", err)
	}
	if strings.Contains(customer, "0.83") || strings.Contains(customer, "classifier") {
		t.Errorf("internal blocker information reached the customer prompt:\n%s", customer)
	}
}

// TestRender_TeamPromptAsksWhatPeerTeamsNeedToKnow: the per-project
// team report is the artifact most people actually read, and it asked
// for status — shipped, blocked, queued, who moved — and never for the
// thing a peer team needs: what changed for them.
func TestRender_TeamPromptAsksWhatPeerTeamsNeedToKnow(t *testing.T) {
	evs := []events.NormalizedEvent{{
		Kind: events.KindMR, Action: "merged", Ref: "#1",
		Title: "Ship the exporter", Updated: time.Now(),
	}}
	_, user, err := Render(AudienceTeam, "g/p", evs, 200, nil, Hint{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"peer team", "changed interface", "deprecation"} {
		if !strings.Contains(strings.ToLower(user), want) {
			t.Errorf("the team prompt does not mention %q:\n%s", want, user)
		}
	}
	// The closing ask is unconditional. "What a peer team needs to know"
	// is a LENS on the events the model already has — a deprecation is
	// visible in a title — so it does not need a producer the way
	// "what is blocked" does. Gating it on the classifier's score
	// would make the report's quality depend on a noisy probability.
	if !strings.Contains(user, "and what a peer team needs to know") {
		t.Errorf("the closing ask drops the peer question without a hint:\n%s", user)
	}
}
