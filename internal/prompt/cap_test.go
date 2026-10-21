package prompt

import (
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
)

// ev builds a minimal event with a known update time.
func ev(kind events.Kind, ref string, updated time.Time) events.NormalizedEvent {
	return events.NormalizedEvent{Kind: kind, Ref: ref, Updated: updated, Title: ref}
}

func at(day int) time.Time { return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC) }

// TestCapEvents_KeepsMostRecentNotFirstN: the pipeline concatenates
// MRs, then issues, then commits. A naive head-slice would drop the
// tail of the commits bucket no matter how recent it is.
func TestCapEvents_KeepsMostRecentNotFirstN(t *testing.T) {
	evs := []events.NormalizedEvent{
		ev(events.KindMR, "#1", at(1)), // oldest
		ev(events.KindIssue, "#2", at(2)),
		ev(events.KindCommit, "aaa", at(3)),
		ev(events.KindCommit, "bbb", at(4)), // newest
	}
	kept, total, dropped := capEvents(evs, 2)
	if len(kept) != 2 || total != 4 || dropped != 2 {
		t.Fatalf("kept=%d total=%d dropped=%d, want 2/4/2", len(kept), total, dropped)
	}
	if kept[0].Ref != "bbb" || kept[1].Ref != "aaa" {
		t.Errorf("kept %s,%s — want the two most recent (bbb,aaa)", kept[0].Ref, kept[1].Ref)
	}
}

// TestCapEvents_CommitsNotStarved: the specific starvation the
// concatenation order caused. MRs + issues fill the budget, but the
// newest events are commits.
func TestCapEvents_CommitsNotStarved(t *testing.T) {
	var evs []events.NormalizedEvent
	for i := 1; i <= 5; i++ { // 5 MRs, oldest to newest
		evs = append(evs, ev(events.KindMR, "#"+string(rune('0'+i)), at(10+i)))
	}
	for i := 1; i <= 30; i++ { // 30 commits, newest at day 1
		evs = append(evs, ev(events.KindCommit, "c"+string(rune('a'+i%26))+string(rune('a'+i/26)), at(40-i)))
	}
	kept, _, _ := capEvents(evs, 10)
	commits := 0
	for _, e := range kept {
		if e.Kind == events.KindCommit {
			commits++
		}
	}
	if commits == 0 {
		t.Errorf("all %d kept events were non-commits; newest commits were starved", len(kept))
	}
}

// TestCapEvents_DoesNotMutateInput: the pipeline passes the same slice
// to both audiences and then re-reads it for the epic rollup. Sorting
// in place would silently reorder that shared state.
func TestCapEvents_DoesNotMutateInput(t *testing.T) {
	evs := []events.NormalizedEvent{
		ev(events.KindCommit, "old", at(1)),
		ev(events.KindCommit, "new", at(9)),
	}
	before := append([]events.NormalizedEvent(nil), evs...)
	capEvents(evs, 1)
	if evs[0].Ref != before[0].Ref || evs[1].Ref != before[1].Ref {
		t.Errorf("capEvents reordered the caller's slice: %s,%s → %s,%s",
			before[0].Ref, before[1].Ref, evs[0].Ref, evs[1].Ref)
	}
}

// TestCapEvents_UnderCapIsUnchanged: no truncation, but still sorted.
func TestCapEvents_UnderCapIsUnchanged(t *testing.T) {
	evs := []events.NormalizedEvent{ev(events.KindMR, "#1", at(3)), ev(events.KindMR, "#2", at(1))}
	kept, total, dropped := capEvents(evs, 10)
	if len(kept) != 2 || total != 2 || dropped != 0 {
		t.Errorf("kept=%d total=%d dropped=%d, want 2/2/0", len(kept), total, dropped)
	}
	// Non-positive cap means "no cap", per the pre-existing contract.
	if kept, _, _ := capEvents(evs, 0); len(kept) != 2 {
		t.Errorf("cap=0 should mean unlimited, got %d", len(kept))
	}
}

// TestCapEvents_DeterministicTieBreak: equal timestamps must sort by
// kind+ref so two runs of the same window produce byte-identical prompts.
func TestCapEvents_DeterministicTieBreak(t *testing.T) {
	evs := []events.NormalizedEvent{
		ev(events.KindCommit, "zzz", at(5)),
		ev(events.KindCommit, "aaa", at(5)),
		ev(events.KindMR, "#9", at(5)),
	}
	first, _, _ := capEvents(evs, 2)
	second, _, _ := capEvents(evs, 2)
	if first[0].Ref != second[0].Ref || first[1].Ref != second[1].Ref {
		t.Error("capEvents is not deterministic across identical runs")
	}
}
