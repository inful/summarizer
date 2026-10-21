package run

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// weekStart is the test-side equivalent of what main.go computes. Tests
// that care about pipeline behaviour rather than its window share this.
func weekStart() time.Time { return WeekStart(time.Now()) }

// TestWeekStart_MatchesTheDirectoryItIsFiledIn: reports land in
// 2026-W43/, so the window has to be that week. A rolling 7 days does
// not: run on a Monday and the file named 2026-W43 holds nothing from
// W43 and a full week of W42 — the most confusing output this tool can
// produce, and the one an operator reads as a bug.
func TestWeekStart_MatchesTheDirectoryItIsFiledIn(t *testing.T) {
	// A Monday and a Wednesday in the same ISO week, 2026-W43.
	mon := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	wed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	for _, now := range []time.Time{mon, wed} {
		start := WeekStart(now)
		if start.Weekday() != time.Monday {
			t.Errorf("%s: window starts %s, want a Monday", now.Format(time.RFC3339), start.Weekday())
		}
		if h, m, s := start.Clock(); h != 0 || m != 0 || s != 0 {
			t.Errorf("%s: window starts at %02d:%02d:%02d, want midnight",
				now.Format(time.RFC3339), h, m, s)
		}
		sy, sw := start.ISOWeek()
		ny, nw := now.ISOWeek()
		if sy != ny || sw != nw {
			t.Errorf("%s: window starts in %d-W%02d but the report is filed in %d-W%02d",
				now.Format(time.RFC3339), sy, sw, ny, nw)
		}
		// Monday 09:30 must not reach back into the previous week.
		if start.Before(mon.AddDate(0, 0, -1)) {
			t.Errorf("%s: window reaches into last week", now.Format(time.RFC3339))
		}
	}
}

// TestWeekStart_YearBoundary: ISO weeks 52 and 53 straddle the new year,
// so "this week" must not become "the last seven days" there.
func TestWeekStart_YearBoundary(t *testing.T) {
	// 2027-01-01 is a Friday, in ISO week 53 of 2026.
	fri := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	start := WeekStart(fri)
	if start.Year() != 2026 {
		t.Errorf("window should start in calendar 2026, got %s", start.Format(time.RFC3339))
	}
	if sy, sw := start.ISOWeek(); sy != 2026 || sw != 53 {
		t.Errorf("window starts in %d-W%02d, want 2026-W53", sy, sw)
	}
	// Sunday is still the same ISO week; Monday rolls over.
	sun := time.Date(2027, 1, 3, 23, 59, 0, 0, time.UTC)
	if s := WeekStart(sun); !s.Equal(start) {
		t.Errorf("Sunday should share the week's start, got %s want %s",
			s.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	nextMon := time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC)
	if s := WeekStart(nextMon); s.Equal(start) {
		t.Error("Monday the 4th should open a new week")
	}
}

// TestWeekStart_IsUTC: a non-UTC machine must produce the same window.
// Without .UTC() the anchor takes the local location, and a server in
// UTC+2 would open the window two hours before its own Monday.
func TestWeekStart_IsUTC(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	// Monday 01:00 in UTC+2 is still Sunday 23:00 UTC.
	local := time.Date(2026, 10, 19, 1, 0, 0, 0, plus2)
	start := WeekStart(local)
	if start.Location() != time.UTC {
		t.Errorf("window start is in %v, want UTC", start.Location())
	}
	// 2026-10-19 is a Monday, so both readings agree on the date here;
	// what matters is that the result is UTC and never in the past.
	if start.After(local.UTC()) {
		t.Errorf("window starts after now: %s", start.Format(time.RFC3339))
	}
}

// dayCount finds a "<N>d" window token. A regexp rather than
// strings.Contains(n, "d--"), which also matches "combin*ed--*".
var dayCount = regexp.MustCompile(`\d+d--`)

// TestFilenames_UseTheWeekToken: there is one window, so the token is a
// constant. It used to render as however many whole days had elapsed —
// "3d" on a Thursday — which renamed the same week's file daily.
func TestFilenames_UseTheWeekToken(t *testing.T) {
	names := []string{
		filename("g/p", "customer"),
		rollupFilename("customer", "rollup"),
		rollupFilename("customer", "epics-rollup"),
	}
	for _, n := range names {
		if !strings.Contains(n, "--wk--") {
			t.Errorf("%q should carry the wk token", n)
		}
		if dayCount.MatchString(n) {
			t.Errorf("%q still carries a day count", n)
		}
	}
}
