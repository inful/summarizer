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
			"group@sub@proj--wk--customer.md"},
		{"per-project team", filename("group/sub/proj", prompt.AudienceTeam),
			"group@sub@proj--wk--team.md"},
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

// TestSanitize_PathSeparatorsBecomeAtSigns: "/" becomes "@", a
// character a GitLab project path cannot contain.
func TestSanitize_PathSeparatorsBecomeAtSigns(t *testing.T) {
	if got := sanitize("group/sub/proj"); got != "group@sub@proj" {
		t.Errorf("got %q, want group@sub@proj", got)
	}
	if got := sanitize("group.sub proj"); got != "group.subproj" {
		t.Errorf("space dropped, dot kept: got %q", got)
	}
	if got := sanitize("///"); got != "@@@" {
		t.Errorf("slashes map to @: got %q, want @@@", got)
	}
	// "." is legal in a GitLab path and legal in a filename, so it is
	// kept rather than dropped.
	if got := sanitize("..."); got != "..." {
		t.Errorf("dots must be kept: got %q", got)
	}
	// Only a fully-empty result (nothing left to write) falls back.
	if got := sanitize("a b"); got != "ab" {
		t.Errorf("all-punctuation input should fall back to %q, got %q", "report", got)
	}
}

// TestSanitize_IsInjective: the property the whole scheme rests on.
// Two paths that sanitize to one name write one file — the second run
// overwrites the first, and then reads the first's docbuilder UID back
// out of it. The customer report is published public: true.
//
// Every pair below is a legal GitLab path that collided under the old
// mapping. The rejected alternatives are recorded in sanitize's comment;
// both still collide on at least one of these.
func TestSanitize_IsInjective(t *testing.T) {
	paths := []string{
		"group/a.b", "group/ab", // dropped "."
		"group/a_b", "group/a/b", // "_" vs separator
		"a/_", "a_/", // prefix-free escape fails
		"team/api.v2", "team/apiv2", // dropped "."
		"g/sub.v2/x", "g/subv2/x", "g/sub_v2/x",
		"a/b.c", "a/bc", "a_b/c", "a/b_c",
		"x/y.z_w", "x/y_z.w",
	}
	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		got := sanitize(p)
		if prev, dup := seen[got]; dup {
			t.Errorf("COLLISION: %q and %q both sanitize to %q — one report "+
				"overwrites the other and inherits its uid", prev, p, got)
			continue
		}
		seen[got] = p
	}
}
