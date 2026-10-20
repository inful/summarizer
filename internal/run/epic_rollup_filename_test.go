package run

import (
	"testing"
	"time"

	"github.com/inful/summarizer/internal/prompt"
)

// TestEpicRollupFilename_Shape: the cross-project epic rollup's
// filename carries an `epics` segment distinct from the per-project
// rollup so docbuilder operators browsing the period directory
// can tell them apart at a glance.
func TestEpicRollupFilename_Shape(t *testing.T) {
	got := epicRollupFilename(7*24*time.Hour, prompt.AudienceCustomer)
	want := "combined--epics--7d--customer.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEpicRollupFilename_DistinctFromRollup: don't collide with the
// per-project rollup filename. Filename collision would mean one
// overwrites the other on disk.
func TestEpicRollupFilename_DistinctFromRollup(t *testing.T) {
	window := 7 * 24 * time.Hour
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		if rollupFilename(window, aud) == epicRollupFilename(window, aud) {
			t.Errorf("epic rollup filename collides with per-project rollup for %s", aud)
		}
	}
}

// TestEpicRollupFilename_FloorOneDay: sub-24h windows floor to 1d
// in the rendered suffix (matches the existing filename helper);
// protects against negative-day renders from short --since flags.
func TestEpicRollupFilename_FloorOneDay(t *testing.T) {
	got := epicRollupFilename(2*time.Hour, prompt.AudienceTeam)
	want := "combined--epics--1d--team.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
