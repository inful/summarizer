package run

import (
	"testing"
	"time"
)

// TestWeeksInMonth: a month rollup gathers every weekly branch whose
// Monday falls inside the month. The edge cases are the ones that
// silently drop or duplicate a week:
//
//   - A month whose first day is not a Monday still has a week
//     straddling the boundary; that week belongs to the *previous*
//     month and must not be counted here.
//   - A month's last week can extend past the month end; the ISO week
//     it belongs to is the one whose Monday is inside, so it counts.
//   - Week 1 of a year can start in late December, so month-to-week
//     iteration must not assume week numbers align with months.
func TestWeeksInMonth(t *testing.T) {
	cases := []struct {
		name  string
		month string // YYYY-MM
		want  []string
	}{
		{
			// Oct 2026: Oct 1 is a Thursday, so the week of Sep 28
			// straddles in and belongs to September. Oct starts on
			// Mon Oct 5 (W41). Mondays: 5,12,19,26 → W41..W44.
			name: "october 2026", month: "2026-10",
			want: []string{"2026-W41", "2026-W42", "2026-W43", "2026-W44"},
		},
		{
			// Sep 2026: Sep 1 is a Tuesday. Mondays in September:
			// Sep 7,14,21,28 → W37..W40. The week of Aug 31 belongs
			// to August and must not appear here.
			name: "september 2026", month: "2026-09",
			want: []string{"2026-W37", "2026-W38", "2026-W39", "2026-W40"},
		},
		{
			// Feb 2026: 28 days, Feb 1 is a Sunday. Mondays: 2,9,16,23
			// → W6..W9.
			name: "february 2026", month: "2026-02",
			want: []string{"2026-W06", "2026-W07", "2026-W08", "2026-W09"},
		},
		{
			// Jan 2024 starts on a Monday and has 31 days → exactly
			// W01..W05.
			name: "january 2024", month: "2024-01",
			want: []string{"2024-W01", "2024-W02", "2024-W03", "2024-W04", "2024-W05"},
		},
		{
			// 2026 is a 53-week ISO year: Dec 28 is a Monday and that
			// week runs into January 2027. Must not choke on W53, and
			// must not spill 2027-W01 into December.
			name: "december 2026 has a 53rd week", month: "2026-12",
			want: []string{"2026-W50", "2026-W51", "2026-W52", "2026-W53"},
		},
		{
			// Consecutive months must not share a week: September ends
			// on W40's Monday (Sep 28), October starts on W41's Monday
			// (Oct 5). A shared week would double-count it.
			name: "june 2026", month: "2026-06",
			want: []string{"2026-W23", "2026-W24", "2026-W25", "2026-W26", "2026-W27"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := weeksInMonth(c.month)
			if err != nil {
				t.Fatalf("weeksInMonth(%q): %v", c.month, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("week %d: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestWeeksInMonth_RejectsBadInput: the month comes from a CLI flag,
// so a typo must fail loudly rather than rolling up nothing.
func TestWeeksInMonth_RejectsBadInput(t *testing.T) {
	bad := []string{"", "2026", "2026-13", "2026-00", "26-10", "2026-1", "2026-10-01", "last-month", "2026/10"}
	for _, m := range bad {
		t.Run(m, func(t *testing.T) {
			if _, err := weeksInMonth(m); err == nil {
				t.Errorf("weeksInMonth(%q) should error", m)
			}
		})
	}
}

// TestPreviousMonth: --month defaults to the month before now, so a
// cron on the 1st rolls up the month that just ended. Year boundary
// is the interesting case: running in January must report the
// previous December of the *previous* year.
func TestPreviousMonth(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"mid-month", time.Date(2026, 10, 21, 9, 0, 0, 0, utc), "2026-09"},
		{"first of month", time.Date(2026, 10, 1, 0, 5, 0, 0, utc), "2026-09"},
		{"january rolls up december", time.Date(2027, 1, 3, 12, 0, 0, 0, utc), "2026-12"},
		{"december rolls up november", time.Date(2026, 12, 15, 12, 0, 0, 0, utc), "2026-11"},
		{"march rolls up february", time.Date(2026, 3, 31, 23, 59, 0, 0, utc), "2026-02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := previousMonth(c.now); got != c.want {
				t.Errorf("previousMonth(%s) = %q, want %q", c.now.Format("2006-01-02"), got, c.want)
			}
		})
	}
}

// TestMonthlySlug_DistinctFromWeekly: the monthly rollup publishes to
// its own period bucket. It must not collide with a weekly one, or
// the monthly file would land on top of a week's MR.
func TestMonthlySlug_DistinctFromWeekly(t *testing.T) {
	if monthlySlug("2026-09") == weekSlugFor(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)) {
		t.Error("monthly and weekly slugs must not collide")
	}
	if got := monthlySlug("2026-09"); got != "2026-09" {
		t.Errorf("monthlySlug = %q, want 2026-09", got)
	}
}
