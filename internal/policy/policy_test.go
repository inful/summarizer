package policy

import (
	"testing"
)

// TestClassify: the nine-row truth table for the summarizer audit-policy
// decision. Drives runOneProject's audience loop, runRollup's customer
// filter, and runEpicRollup's customer filter. Truth table copied from
// docs/deferred.md once that's flipped to "shipped".
func TestClassify(t *testing.T) {
	type tc struct {
		name         string
		source       ProjectSource
		topics       []string
		inList       bool // in summarizer.yaml internal_only_projects OR on --internal-only-project
		wantTeam     bool
		wantCustomer bool
	}
	cases := []tc{
		// Explicit-listed: operator's "I trust this" default.
		{"explicit, no topics, no override", SourceExplicit, nil, false, false, true},
		{"explicit, summarizer topic, no override", SourceExplicit, []string{"summarizer"}, false, false, true},
		{"explicit, summarizer-team-only, no override", SourceExplicit, []string{"summarizer-team-only"}, false, true, false},
		{"explicit, both topics", SourceExplicit, []string{"summarizer", "summarizer-team-only"}, false, true, false},
		{"explicit, irrelevant topic", SourceExplicit, []string{"ruby", "kubernetes"}, false, false, true},

		// Discovered: default safe (team-only unless summarizer).
		{"discovered, summarizer topic", SourceDiscovered, []string{"summarizer"}, false, false, true},
		{"discovered, summarizer-team-only", SourceDiscovered, []string{"summarizer-team-only"}, false, true, false},
		{"discovered, both topics", SourceDiscovered, []string{"summarizer", "summarizer-team-only"}, false, true, false},
		{"discovered, no topics", SourceDiscovered, nil, false, true, false},
		{"discovered, irrelevant topic", SourceDiscovered, []string{"ruby"}, false, true, false},

		// Override (config / CLI) — always wins.
		{"explicit + override", SourceExplicit, []string{"summarizer"}, true, true, false},
		{"discovered + override, no topics", SourceDiscovered, nil, true, true, false},
		{"discovered + override, summarizer topic", SourceDiscovered, []string{"summarizer"}, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Classify(c.source, c.topics, c.inList)
			if p.TeamOnly != c.wantTeam {
				t.Errorf("TeamOnly: got %v, want %v", p.TeamOnly, c.wantTeam)
			}
			if p.CustomerOn != c.wantCustomer {
				t.Errorf("CustomerOn: got %v, want %v", p.CustomerOn, c.wantCustomer)
			}
			// Both policies must always run in v1 (no per-project skip).
			if !p.Run {
				t.Errorf("Run should always be true in v1; got false")
			}
		})
	}
}

// TestClassify_TopicCaseSensitive: GitLab topics are case-sensitive but the
// way the API returns them is consistent. We follow exactly the case
// the operator sets in GitLab. This pins that case-insensitive matching
// is NOT a feature — operators who write `Summarizer` get a different
// result from `summarizer`.
func TestClassify_TopicCaseSensitive(t *testing.T) {
	lowercase := Classify(SourceDiscovered, []string{"summarizer"}, false)
	mixedcase := Classify(SourceDiscovered, []string{"Summarizer"}, false)
	if lowercase.CustomerOn != true {
		t.Errorf("lowercase 'summarizer' should opt-in to customer")
	}
	if mixedcase.CustomerOn != false {
		t.Errorf("'Summarizer' (capitalized) should NOT opt-in; topics are case-sensitive")
	}
}

// TestHasTopic is the small lookup the classifier depends on. Pins that
// the topic must appear as the whole token, not as a prefix or a
// substring. (GitLab topics are tokens, so prefix-matching would be
// wrong.) Catches a future refactor that swaps to a contains-based
// check.
func TestHasTopic(t *testing.T) {
	cases := []struct {
		topics []string
		want   string
		out    bool
	}{
		{[]string{"summarizer"}, "summarizer", true},
		{[]string{"summarizer-team-only"}, "summarizer-team-only", true},
		{[]string{"summarizer-team-only", "ruby"}, "summarizer", false},
		{[]string{"ruby", "summarizer"}, "summarizer", true},
		{nil, "summarizer", false},
		{[]string{}, "summarizer", false},
	}
	for _, c := range cases {
		got := hasTopic(c.topics, c.want)
		if got != c.out {
			t.Errorf("hasTopic(%v, %q) = %v, want %v", c.topics, c.want, got, c.out)
		}
	}
}
