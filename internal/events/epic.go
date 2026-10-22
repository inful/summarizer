package events

import (
	"strconv"
	"strings"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// AttachEpics annotates each Issue-kind event with the Epic it's part
// of, building a lookup from issue to epic. Other event kinds
// (MR, Commit) are deliberately left alone — commits don't carry
// issue linkage in this code path, and MR-closes-issue correlation
// would require a deeper API surface that summarizer doesn't fetch
// today. If a future operator wants MRs grouped, that's a separate
// cross-reference fetch.
//
// project is the project whose events these are, and it is load-bearing
// rather than documentation. Issue IIDs are unique per PROJECT, while
// epics are group-scoped: `/groups/:id/epics/:iid/issues` returns every
// issue in the epic across the whole group. Keyed on the IID alone,
// project A's issue #10 and project B's issue #10 collide, and A's work
// gets filed under whichever epic B's issue happens to sit in — which
// then mis-counts the epic's activity and can attribute one customer's
// work to another, in a file published public: true.
//
// An issue with no project reference cannot be placed in this project
// and is skipped. That is the safe direction: GitLab has sent
// references.full on this endpoint since 11.0, and a mis-attribution
// costs a customer their own work.
//
// The comparison is case-insensitive because GitLab project routes
// are. `--project Group/Sub/App` reaches the same project as
// `group/sub/app` and works for MRs, issues, commits and discovery —
// so an exact compare would have made the epic section the one place
// where the operator's spelling mattered, and it would have failed
// silently.
//
// On duplicate (project, IID) across epics — an operator-side mistake
// in GitLab itself — the first epic wins. The cost of ambiguous data is
// just "this issue is grouped under the first epic that claims it" —
// predictable beats undefined.
func AttachEpics(es []NormalizedEvent, epics []*gitlab.Epic, project string) {
	if len(epics) == 0 {
		return
	}
	byIID := make(map[int]*gitlab.Epic, 64)
	for _, e := range epics {
		for _, iss := range e.Issues {
			if !strings.EqualFold(iss.Project(), project) {
				continue
			}
			if _, seen := byIID[iss.IID]; seen {
				continue
			}
			byIID[iss.IID] = e
		}
	}
	for i := range es {
		ev := &es[i]
		if ev.Kind != KindIssue {
			continue
		}
		iid, ok := parseIssueIID(ev.Ref)
		if !ok {
			continue
		}
		if epic, hit := byIID[iid]; hit {
			ev.Epic = epic
		}
	}
}

// parseIssueIID extracts a numeric IID from refs like "#42" or "42".
// Returns (0, false) for anything not parseable so a malformed ref
// silently drops out of correlation rather than panicking.
func parseIssueIID(ref string) (int, bool) {
	s := strings.TrimPrefix(ref, "#")
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// FilterActiveEpics returns the subset of epics that had at least one
// of their linked issues with updated_at in [since, until].
//
// This runs AFTER the cap, not before it: fetchProjectEpics selects
// the maxActiveEpics most recently updated epics, fetches their issues,
// and only then calls this. An inactive epic that was not selected is
// not "filtered out" — it was never fetched, so it cannot be reported
// on. The cap's shortcut comment says what that costs.
//
// Half-open interval: since ≤ updated_at ≤ until. Issues updated
// exactly at the boundary count as active; this matters for cron
// jobs that fire on the same minute the window closes.
func FilterActiveEpics(epics []*gitlab.Epic, since, until time.Time) []*gitlab.Epic {
	out := make([]*gitlab.Epic, 0, len(epics))
	for _, e := range epics {
		if !epicHasActiveIssue(e, since, until) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func epicHasActiveIssue(e *gitlab.Epic, since, until time.Time) bool {
	if len(e.Issues) == 0 {
		return false
	}
	for _, iss := range e.Issues {
		t := iss.UpdatedAt
		if t.Before(since) || t.After(until) {
			continue
		}
		return true
	}
	return false
}
