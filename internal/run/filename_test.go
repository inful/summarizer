package run

import (
	"testing"

	"github.com/inful/summarizer/internal/prompt"
)

// TestFilenames_Shape: the report kinds a run can publish, and the exact
// names the target repo's period directory ends up with.
func TestFilenames_Shape(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"per-project customer", filename("group/sub/proj", prompt.AudienceCustomer),
			"group_sub_proj--wk--customer.md"},
		{"per-project team", filename("group/sub/proj", prompt.AudienceTeam),
			"group_sub_proj--wk--team.md"},
		{"rollup customer", rollupFilename(prompt.AudienceCustomer, "rollup"),
			"combined--rollup--wk--customer.md"},
		{"rollup team", rollupFilename(prompt.AudienceTeam, "rollup"),
			"combined--rollup--wk--team.md"},
		{"epic rollup customer", rollupFilename(prompt.AudienceCustomer, "epics-rollup"),
			"combined--epics-rollup--wk--customer.md"},
		{"per-customer epic rollup", rollupFilename(prompt.AudienceCustomer, "epics-acme"),
			"combined--epics-acme--wk--customer.md"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestFilenames_NoCollision: a collision means one report overwrites
// another on the same branch. Every shape must be distinct.
func TestFilenames_NoCollision(t *testing.T) {
	names := map[string]string{
		"project/customer": filename("group/sub/proj", prompt.AudienceCustomer),
		"project/team":     filename("group/sub/proj", prompt.AudienceTeam),
		"rollup/customer":  rollupFilename(prompt.AudienceCustomer, "rollup"),
		"rollup/team":      rollupFilename(prompt.AudienceTeam, "rollup"),
		"epics/customer":   rollupFilename(prompt.AudienceCustomer, "epics-rollup"),
		"epics/team":       rollupFilename(prompt.AudienceTeam, "epics-rollup"),
		"acme epic/cust":   rollupFilename(prompt.AudienceCustomer, "epics-acme"),
		"acme epic/team":   rollupFilename(prompt.AudienceTeam, "epics-acme"),
	}
	seen := make(map[string]string, len(names))
	for label, n := range names {
		if other, dup := seen[n]; dup {
			t.Errorf("%q and %s both produce %q", label, other, n)
		}
		seen[n] = label
	}
}

// TestWeeklyRollupName_MatchesWhatThePublisherWrites: the monthly
// reader names the file it wants outright rather than listing the week
// and pattern-matching. These two must stay in step or a month's worth
// of rollups silently reads as empty.
func TestWeeklyRollupName_MatchesWhatThePublisherWrites(t *testing.T) {
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		if got, want := weeklyRollupName(aud), rollupFilename(aud, "rollup"); got != want {
			t.Errorf("%s: monthly reads %q, publisher writes %q", aud, got, want)
		}
	}
}

// TestSanitize_PathSeparatorsBecomeUnderscores: two different projects
// whose paths differ only by separator shape must not collide.
func TestSanitize_PathSeparatorsBecomeUnderscores(t *testing.T) {
	if got := sanitize("group/sub/proj"); got != "group_sub_proj" {
		t.Errorf("got %q", got)
	}
	// Dots and other punctuation are dropped, so this collides with
	// "groupXsubYproj" — document the behaviour rather than assert a
	// uniqueness the format can't provide.
	if got := sanitize("group.sub proj"); got != "groupsubproj" {
		t.Errorf("got %q, want %q", got, "groupsubproj")
	}
	if got := sanitize("///"); got != "___" {
		t.Errorf("slashes map to underscores: got %q, want %q", got, "___")
	}
	// Only a fully-empty result (no alphanumerics at all) falls back.
	if got := sanitize("..."); got != "report" {
		t.Errorf("all-punctuation input should fall back to %q, got %q", "report", got)
	}
}
