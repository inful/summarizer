package run

import (
	"testing"
	"time"
)

// TestWeekWindow_MatchesTheDirectoryItIsFiledIn: reports land in
// 2026-W43/, so the window has to be that week. A rolling 7 days does
// not: run on a Monday and the file named 2026-W43 holds nothing from
// W43 and a full week of W42 — the single most confusing output this
// tool can produce, and the one an operator reads as a bug.
func TestWeekWindow_MatchesTheDirectoryItIsFiledIn(t *testing.T) {
	// A Wednesday and a Monday in the same ISO week, 2026-W43.
	wed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	mon := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"Monday 09:30 has nothing yet", mon, 9*time.Hour + 30*time.Minute},
		{"Wednesday noon is 2d12h in", wed, 60 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, scoped := WeekWindow(tc.now)
			if !scoped {
				t.Error("WeekWindow must report that the window is week-scoped")
			}
			if got != tc.want {
				t.Errorf("window = %s, want %s", got, tc.want)
			}
			// The window's start must land on the same ISO week as the
			// period directory, or the two disagree.
			start := tc.now.Add(-got)
			wy, ww := start.ISOWeek()
			ny, nw := tc.now.ISOWeek()
			if wy != ny || ww != nw {
				t.Errorf("window starts in %d-W%02d but the report is filed in %d-W%02d",
					wy, ww, ny, nw)
			}
			// And on a Monday, never in the previous week.
			if start.Weekday() != time.Monday {
				t.Errorf("window should start on a Monday, got %s", start.Weekday())
			}
		})
	}
}

// TestWeekWindow_YearBoundary: ISO weeks 52 and 53 straddle the new
// year, and "this week" must not become "the last seven days" there.
func TestWeekWindow_YearBoundary(t *testing.T) {
	// 2027-01-01 is a Friday, in ISO week 53 of 2026.
	fri := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	got, _ := WeekWindow(fri)
	start := fri.Add(-got)
	sy, sw := start.ISOWeek()
	fy, fw := fri.ISOWeek()
	if sy != fy || sw != fw {
		t.Errorf("window starts in %d-W%02d but the report is filed in %d-W%02d", sy, sw, fy, fw)
	}
	if start.Year() != 2026 {
		t.Errorf("window should start in calendar 2026, got %s", start.Format(time.RFC3339))
	}
}

// TestWindowToken: the filename's middle segment. The week default used
// to render as however many whole days had elapsed — "3d" on a Thursday
// — so the same week's file was renamed daily and no two days agreed on
// what it covered. It is "wk" now, and stays "wk" all week.
func TestWindowToken(t *testing.T) {
	thu := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	mon := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	thuWin, _ := WeekWindow(thu)
	monWin, _ := WeekWindow(mon)
	if got := windowToken(true, thuWin); got != "wk" {
		t.Errorf("Thursday window token = %q, want %q", got, "wk")
	}
	if got := windowToken(true, monWin); got != "wk" {
		t.Errorf("Monday window token = %q, want %q — it must not change as the week fills in", got, "wk")
	}
	// An explicit --since still names its length.
	if got := windowToken(false, 30*24*time.Hour); got != "30d" {
		t.Errorf("explicit 30d window token = %q, want %q", got, "30d")
	}
	if got := windowToken(false, 6*time.Hour); got != "1d" {
		t.Errorf("sub-day explicit window token = %q, want %q", got, "1d")
	}
}

// TestFilename_WeekDefault: the end-to-end shape a run writes.
func TestFilename_WeekDefault(t *testing.T) {
	win, _ := WeekWindow(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if got := filename(true, win, "group/proj", "customer"); got != "group_proj--wk--customer.md" {
		t.Errorf("filename = %q", got)
	}
	if got := filename(false, 7*24*time.Hour, "group/proj", "customer"); got != "group_proj--7d--customer.md" {
		t.Errorf("explicit-window filename = %q", got)
	}
}
