package run

import (
	"testing"
	"time"

	"github.com/inful/summarizer/internal/prompt"
)

const week = 7 * 24 * time.Hour

// TestFilenames_Shape: the three report kinds a run can publish, and
// the exact names the target repo's period directory ends up with.
func TestFilenames_Shape(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"per-project customer", filename(false, week, "group/sub/proj", prompt.AudienceCustomer),
			"group_sub_proj--7d--customer.md"},
		{"per-project team", filename(false, week, "group/sub/proj", prompt.AudienceTeam),
			"group_sub_proj--7d--team.md"},
		{"rollup customer", rollupFilename(false, week, prompt.AudienceCustomer, "rollup"),
			"combined--rollup--7d--customer.md"},
		{"epic rollup customer", rollupFilename(false, week, prompt.AudienceCustomer, "epics-rollup"),
			"combined--epics-rollup--7d--customer.md"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestFilenames_NoCollision: a collision means one report overwrites
// another on the same branch. All four shapes must be distinct.
func TestFilenames_NoCollision(t *testing.T) {
	names := map[string]string{
		"project/customer": filename(false, week, "group/sub/proj", prompt.AudienceCustomer),
		"project/team":     filename(false, week, "group/sub/proj", prompt.AudienceTeam),
		"rollup/customer":  rollupFilename(false, week, prompt.AudienceCustomer, "rollup"),
		"rollup/team":      rollupFilename(false, week, prompt.AudienceTeam, "rollup"),
		"epics/customer":   rollupFilename(false, week, prompt.AudienceCustomer, "epics-rollup"),
		"epics/team":       rollupFilename(false, week, prompt.AudienceTeam, "epics-rollup"),
	}
	seen := make(map[string]string, len(names))
	for label, n := range names {
		if other, dup := seen[n]; dup {
			t.Errorf("%q and %s both produce %q", label, other, n)
		}
		seen[n] = label
	}
}

// TestFilenames_FloorOneDay: sub-24h windows floor to 1d so a short
// --since never renders "--0d--".
func TestFilenames_FloorOneDay(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"2h project", filename(false, 2*time.Hour, "g/p", prompt.AudienceTeam), "g_p--1d--team.md"},
		{"2h epic rollup", rollupFilename(false, 2*time.Hour, prompt.AudienceTeam, "epics-rollup"),
			"combined--epics-rollup--1d--team.md"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestWindowDays: the rounding rule behind the filename suffix.
func TestWindowDays(t *testing.T) {
	cases := []struct {
		w    time.Duration
		want int
	}{
		{0, 1},
		{-time.Hour, 1},
		{time.Minute, 1},
		{23 * time.Hour, 1},
		{24 * time.Hour, 1},
		{7 * 24 * time.Hour, 7},
		{30 * 24 * time.Hour, 30},
	}
	for _, c := range cases {
		if got := windowDays(c.w); got != c.want {
			t.Errorf("windowDays(%v) = %d, want %d", c.w, got, c.want)
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
