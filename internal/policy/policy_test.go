package policy

import "testing"

// TestClassify: the truth table for the project-policy decision. Drives
// runOneProject's audience loop, runRollup's customer filter, and
// runEpicRollup's customer filter.
//
// The model inverts from "opt in to summarizer" to "opt out of
// summarizer." Every active project runs by default and produces a
// team-only report; the only way to surface customer-facing content
// is to add the `summarize` topic. The project-owner signal that
// removes the project entirely lives in a different place (the
// .summaryignore file at the repo root, checked at the pipeline
// layer rather than here).
//
// How a project entered the run is not an input: the audience default
// is the same for an explicitly-named project and a discovered one.
func TestClassify(t *testing.T) {
	type tc struct {
		name         string
		topics       []string
		inList       bool // in summarizer.yaml internal_only_projects OR on --internal-only-project
		wantTeam     bool
		wantCustomer bool
	}
	cases := []tc{
		// The only way to opt in to customer output is the
		// `summarize` topic.
		{"summarize topic", []string{"summarize"}, false, false, true},

		// summarize-team-only wins over summarize (sticky override).
		{"team-only topic", []string{"summarize-team-only"}, false, true, false},
		{"both topics, team-only wins", []string{"summarize", "summarize-team-only"}, false, true, false},

		// The default: any project with no `summarize` topic gets a
		// team-only report. This is the behaviour change the operator
		// asked for — the friction of "every project needs a topic
		// to be summarised" is replaced by "every project is
		// summarised unless told otherwise."
		{"no topics", nil, false, true, false},
		{"unrelated topics", []string{"ruby", "kubernetes"}, false, true, false},

		// Operator-level override (config / CLI): always wins.
		{"summarize topic, but operator override", []string{"summarize"}, true, true, false},
		{"no topics, operator override", nil, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Classify(c.topics, c.inList)
			if p.TeamOnly != c.wantTeam {
				t.Errorf("TeamOnly: got %v, want %v", p.TeamOnly, c.wantTeam)
			}
			if p.CustomerOn != c.wantCustomer {
				t.Errorf("CustomerOn: got %v, want %v", p.CustomerOn, c.wantCustomer)
			}
		})
	}
}

// TestClassify_TopicCaseSensitive: GitLab topics are case-sensitive but the
// way the API returns them is consistent. We follow exactly the case
// the operator sets in GitLab. This pins that case-insensitive matching
// is NOT a feature — operators who write `Summarize` get a different
// result from `summarizer`.
func TestClassify_TopicCaseSensitive(t *testing.T) {
	lowercase := Classify([]string{"summarize"}, false)
	mixedcase := Classify([]string{"Summarize"}, false)
	if lowercase.CustomerOn != true {
		t.Errorf("lowercase 'summarize' should opt-in to customer")
	}
	if mixedcase.CustomerOn != false {
		t.Errorf("'Summarize' (capitalized) should NOT opt-in; topics are case-sensitive")
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
		{[]string{"summarize"}, "summarize", true},
		{[]string{"summarize-team-only"}, "summarize-team-only", true},
		{[]string{"summarize-team-only", "ruby"}, "summarize", false},
		{[]string{"ruby", "summarize"}, "summarize", true},
		{nil, "summarize", false},
		{[]string{}, "summarize", false},
	}
	for _, c := range cases {
		got := hasTopic(c.topics, c.want)
		if got != c.out {
			t.Errorf("hasTopic(%v, %q) = %v, want %v", c.topics, c.want, got, c.out)
		}
	}
}
