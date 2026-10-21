package run

import (
	"testing"
	"time"
)

// TestWeekSlug_KnownDates: pin the ISO-8601 week-number output. The
// reference dates are concrete calendar days so the test stays stable
// across timezones and DST shifts.
//
// ISO 8601 week rules that make these non-obvious:
//   - Weeks start Monday, week 1 is the week containing the first
//     Thursday of the year.
//   - Early January can belong to the previous year's last week, and
//     late December to the next year's first week. Go's ISOWeek()
//     implements this; the cases below pin that we use it rather than
//     a naive (day-of-year / 7) approximation.
func TestWeekSlug_KnownDates(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name  string
		input time.Time
		want  string
	}{
		// Ordinary mid-year weeks: every day of a week maps to one slug.
		{"Mon of W42", time.Date(2026, 10, 12, 0, 0, 0, 0, utc), "2026-W42"},
		{"Wed of W42", time.Date(2026, 10, 14, 9, 30, 0, 0, utc), "2026-W42"},
		{"Sun of W42", time.Date(2026, 10, 18, 23, 59, 59, 0, utc), "2026-W42"},
		// Sunday 23:59:59 must NOT roll into the next week; the
		// boundary is the following Monday at 00:00.
		{"Mon of W43", time.Date(2026, 10, 19, 0, 0, 0, 0, utc), "2026-W43"},
		{"Sun of W43", time.Date(2026, 10, 25, 23, 59, 59, 0, utc), "2026-W43"},
		{"Mon of W44", time.Date(2026, 10, 26, 0, 0, 0, 0, utc), "2026-W44"},

		// Year-boundary cases. 2027-01-01 is a Friday; ISO 8601 puts
		// it in 2026-W53 because that week runs Mon 2026-12-28 →
		// Sun 2027-01-03 and contains the first Thursday (2027-01-07
		// falls outside it, so the week belongs to the year that owns
		// the majority of its days).
		{"2027-01-01 belongs to 2026-W53", time.Date(2027, 1, 1, 12, 0, 0, 0, utc), "2026-W53"},
		{"2026-12-31 belongs to 2026-W53", time.Date(2026, 12, 31, 23, 0, 0, 0, utc), "2026-W53"},

		// A year that starts exactly on a Monday still has week 1
		// containing Jan 1, so no -W00 should ever be emitted.
		{"2024-01-01 is 2024-W01", time.Date(2024, 1, 1, 0, 0, 0, 0, utc), "2024-W01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := weekSlugFor(c.input); got != c.want {
				t.Errorf("weekSlugFor(%s) = %q, want %q", c.input.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

// TestWeekSlug_AlwaysSevenDaysACrossAYear: one slug must map to
// exactly one Monday. If the same slug ever spanned two Mondays the
// publisher would reuse a branch across two different weeks, and if
// two Mondays shared a slug one week's reports would overwrite the
// other's. Walks every day of 2026 plus the 2026/2027 boundary.
func TestWeekSlug_AlwaysSevenDaysACrossAYear(t *testing.T) {
	utc := time.UTC
	mondayBySlug := map[string]time.Time{}
	d := time.Date(2026, 1, 1, 12, 0, 0, 0, utc)
	end := time.Date(2027, 1, 15, 0, 0, 0, 0, utc)
	for d.Before(end) {
		slug := weekSlugFor(d)
		monday := mondayAnchor(d)
		if prev, seen := mondayBySlug[slug]; seen && !prev.Equal(monday) {
			t.Fatalf("slug %q spans two Mondays: %s and %s",
				slug, prev.Format("2006-01-02"), monday.Format("2006-01-02"))
		}
		mondayBySlug[slug] = monday
		d = d.AddDate(0, 0, 1)
	}
	// 2026-01-01 through 2027-01-14 is ~379 days ≈ 55 ISO weeks. A
	// sanity bound, not an exact count, so this doesn't break on a
	// future ISO calendar change.
	if n := len(mondayBySlug); n < 50 || n > 60 {
		t.Errorf("expected roughly 55 weekly buckets, got %d", n)
	}
}

// TestPeriodSlug_IsWeekNumber: periodSlug is the name the publisher
// derives the branch and directory from. It must be a week slug and
// must stay stable across every run within the same week — that's the
// whole reason day 2 overwrites day 1's branch instead of forking a
// new one.
func TestPeriodSlug_IsWeekNumber(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	got := weekSlugFor(now)
	if got != "2026-W42" {
		t.Errorf("weekSlugFor = %q, want 2026-W42", got)
	}
	// Same week, later day: identical slug.
	if later := weekSlugFor(now.AddDate(0, 0, 4)); later != got {
		t.Errorf("Sunday of the same week gave %q, want %q", later, got)
	}
	// Next week: new slug.
	if next := weekSlugFor(now.AddDate(0, 0, 7)); next != "2026-W43" {
		t.Errorf("next week gave %q, want 2026-W43", next)
	}
}

// TestOptions_ForceOverwrite: every run lands in the same weekly
// bucket, so a refuse-first local publisher would fail the cron from
// day 2 onward. Force is implied; --force is no longer needed to
// overwrite within a week.
func TestOptions_ForceOverwrite(t *testing.T) {
	if (Options{}).forceOverwrite() != true {
		t.Error("weekly buckets always imply overwrite; day 2 must not fail refuse-first")
	}
}
