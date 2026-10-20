package events

import (
	"strconv"
	"strings"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// AttachEpics annotates each Issue-kind event with the Epic it's part
// of, building a lookup from issue IID to epic. Other event kinds
// (MR, Commit) are deliberately left alone — commits don't carry
// issue linkage in this code path, and MR-closes-issue correlation
// would require a deeper API surface that summarizer doesn't fetch
// today. If a future operator wants MRs grouped, that's a separate
// cross-reference fetch.
//
// On duplicate IID across epics (an operator-side mistake in GitLab
// itself), the first epic wins. The cost of ambiguous data is just
// "this issue is grouped under the first epic that claims it" —
// predictable beats undefined.
func AttachEpics(es []NormalizedEvent, epics []*gitlab.Epic) {
	if len(epics) == 0 {
		return
	}
	byIID := make(map[int]*gitlab.Epic, 64)
	for _, e := range epics {
		for _, iss := range e.Issues {
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
// of their linked issues with updated_at in [since, until]. The cap
// on rendered epics is applied to this filtered slice, not the whole
// input — a project's "inactive" epics never appear in the report,
// regardless of how many exist.
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
