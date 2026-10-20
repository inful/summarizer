package run

import (
	"testing"
	"time"
)

// TestMondayAnchor_KnownDates: pin the date-arithmetic that anchors a
// given timestamp to the most-recent Monday at-or-before. The
// reference dates are concrete calendar days so the test stays
// stable across timezones and DST shifts.
func TestMondayAnchor_KnownDates(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name   string
		input  time.Time
		expect time.Time
	}{
		// ISO weeks: 2026-W42 starts Mon 2026-10-12 (Oct 12 is the Monday
		// of week 42). Oct 13 (Tue), Oct 14 (Wed), Oct 18 (Sun) all
		// anchor back to Monday Oct 12.
		{"Mon Oct 12 of W42", time.Date(2026, 10, 12, 14, 30, 0, 0, utc), time.Date(2026, 10, 12, 0, 0, 0, 0, utc)},
		{"Tue Oct 13 of W42", time.Date(2026, 10, 13, 9, 0, 0, 0, utc), time.Date(2026, 10, 12, 0, 0, 0, 0, utc)},
		{"Wed Oct 14 of W42", time.Date(2026, 10, 14, 9, 0, 0, 0, utc), time.Date(2026, 10, 12, 0, 0, 0, 0, utc)},
		{"Sun Oct 18 of W42", time.Date(2026, 10, 18, 23, 59, 0, 0, utc), time.Date(2026, 10, 12, 0, 0, 0, 0, utc)},

		// Week boundary crossing. Oct 19 (Mon) is week 43 in 2026.
		// It should NOT anchor back to Oct 12; it should land on
		// itself.
		{"Mon Oct 19 starts W43", time.Date(2026, 10, 19, 0, 0, 1, 0, utc), time.Date(2026, 10, 19, 0, 0, 0, 0, utc)},

		// Sunday wrap-around (Sunday shifts back 6 days).
		{"Sun Nov 1 2026 still W44", time.Date(2026, 11, 1, 12, 0, 0, 0, utc), time.Date(2026, 10, 26, 0, 0, 0, 0, utc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mondayAnchor(c.input)
			if !got.Equal(c.expect) {
				t.Errorf("mondayAnchor(%s) = %s, want %s", c.input.Format(time.RFC3339), got.Format(time.RFC3339), c.expect.Format(time.RFC3339))
			}
		})
	}
}

// TestPeriodSlug_DailyVsMonday: the period-slug function picks the
// right format string based on PeriodMode. Daily always uses
// today's date; Monday-anchored uses the most-recent Monday.
//
// Pinning daily output: the slug equals today's date string.
// Pinning monday output: the slug equals the mondayAnchor's
// formatted date. Three known timestamps cover:
func TestPeriodSlug_DailyVsMonday(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	if got := periodSlugFor(now, PeriodModeDaily); got != "2026-10-14" {
		t.Errorf("daily: got %q, want 2026-10-14", got)
	}
	// Tuesday Oct 13 2026 → Monday Oct 12 2026 (W42 anchor).
	if got := periodSlugFor(now, PeriodModeMonday); got != "2026-10-12" {
		t.Errorf("monday: got %q, want 2026-10-12", got)
	}
}

// TestPeriodMode_ParseAndValidate: parses the CLI string into the
// enum. Strict (rejects unknown values) since typos here mean
// silent behavior drift.
func TestPeriodMode_ParseAndValidate(t *testing.T) {
	cases := []struct {
		in   string
		want PeriodMode
		err  bool
	}{
		{"daily", PeriodModeDaily, false},
		{"monday", PeriodModeMonday, false},
		{"", PeriodModeDaily, false}, // empty defaults to daily
		{"weekly", "", true},
		{"d", "", true},
		{"Daily", "", true}, // case-sensitive; matches stringer
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParsePeriodMode(c.in)
			if c.err {
				if err == nil {
					t.Errorf("expected error for %q, got %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestOptions_ForceOverwrite: monday mode implies Force (the whole
// point of the weekly bucket is that day 2 overwrites day 1), while
// daily mode still refuses by default.
func TestOptions_ForceOverwrite(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"daily, no force", Options{PeriodMode: PeriodModeDaily}, false},
		{"unset mode, no force", Options{}, false},
		{"monday implies force", Options{PeriodMode: PeriodModeMonday}, true},
		{"daily + explicit force", Options{PeriodMode: PeriodModeDaily, Force: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.opts.forceOverwrite(); got != c.want {
				t.Errorf("forceOverwrite() = %v, want %v", got, c.want)
			}
		})
	}
}
