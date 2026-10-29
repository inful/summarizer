package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/safety"
)

// labelSweepServer models the case that broke keepNewestMRs: a week
// with far more merges than max_events, plus older archived MRs that
// GitLab reports as recently updated because a label changed.
type labelSweepServer struct {
	mergedThisWeek int
	relabelledOld  int
	// Atomic: fillMRDetail fetches /changes from four goroutines.
	changeCalls atomic.Int64
}

func (s *labelSweepServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/merge_requests/") && strings.HasSuffix(p, "/changes"):
			s.changeCalls.Add(1)
			_, _ = w.Write([]byte(`{"changes_count":"1","diffs":[{"diff":"+a\n+b"}]}`))
		case strings.Contains(p, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(p, "/merge_requests"):
			now := time.Now().UTC()
			var list []map[string]any
			iid := 0
			// Merged this week: merged_at AND updated_at both recent.
			for n := 0; n < s.mergedThisWeek; n++ {
				iid++
				list = append(list, map[string]any{
					"iid": iid, "title": "merged", "state": "merged",
					"updated_at": now.Add(-time.Duration(n) * time.Hour).Format(time.RFC3339),
					"merged_at":  now.Add(-time.Duration(n) * time.Hour).Format(time.RFC3339),
				})
			}
			// Archived months ago, but a label changed today: updated_at
			// is recent and merged_at is not.
			for n := 0; n < s.relabelledOld; n++ {
				iid++
				list = append(list, map[string]any{
					"iid": iid, "title": "archived", "state": "merged",
					"updated_at": now.Add(-time.Duration(n) * time.Minute).Format(time.RFC3339),
					"merged_at":  now.AddDate(0, -3, 0).Add(-time.Duration(n) * time.Minute).Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.Contains(p, "/issues"), strings.Contains(p, "/repository/commits"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[]}`))
		}
	})
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	return mux
}

// TestKeepNewestMRs_MatchesTheCapItFeeds: prompt.capEvents sorts on the
// event's timestamp, which events.MR derives from merged_at or
// closed_at — not GitLab's updated_at, which a label change also
// bumps. Sorting the trim on updated_at meant a housekeeping label
// sweep over archived MRs displaced the week's real merges, and the
// report showed a fraction of the work that happened.
func TestKeepNewestMRs_MatchesTheCapItFeeds(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var mrs []gitlab.MergeRequest
	iid := 0
	for n := 0; n < 20; n++ { // 20 merged this week
		iid++
		mrs = append(mrs, gitlab.MergeRequest{
			IID:       iid,
			State:     "merged",
			UpdatedAt: now.Add(-time.Duration(n) * time.Hour),
			MergedAt:  ptrTime(now.Add(-time.Duration(n) * time.Hour)),
		})
	}
	for n := 0; n < 30; n++ { // 30 archived, re-labelled today
		iid++
		mrs = append(mrs, gitlab.MergeRequest{
			IID:       iid,
			State:     "merged",
			UpdatedAt: now.Add(-time.Duration(n) * time.Minute),
			MergedAt:  ptrTime(now.AddDate(0, -3, 0).Add(-time.Duration(n) * time.Minute)),
		})
	}

	kept, dropped := keepNewestMRs(mrs, 20)
	if dropped != 30 {
		t.Errorf("dropped = %d, want 30", dropped)
	}
	for _, m := range kept {
		if m.MergedAt.Before(now.AddDate(0, 0, -7)) {
			t.Errorf("kept archived MR #%d merged %s; the week's merges must survive",
				m.IID, m.MergedAt.Format(time.RFC3339))
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestRunOneProject_LabelSweepDoesNotDisplaceThisWeeksMerges: the
// end-to-end version. The /changes fetch must cover this week's merges,
// because those are what the prompt will describe.
func TestRunOneProject_LabelSweepDoesNotDisplaceThisWeeksMerges(t *testing.T) {
	srv := &labelSweepServer{mergedThisWeek: 30, relabelledOld: 40}
	hs := httptest.NewServer(srv.handler())
	defer hs.Close()

	p := &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 30},
		GitLab:    gitlab.New(hs.URL, "glpat-x", 5*time.Second),
		LLM:       &failingLLM{},
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
		Opts:      Options{Since: weekStart(), Period: WeekSlug(time.Now()), Projects: []string{"g/p"}},
	}
	if _, err := p.runOneProject(context.Background(), "g/p"); err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	// 30 merged + 40 relabelled = 70 MRs, cap 30. Every /changes call
	// should be on a merged-this-week MR, so the count must be exactly
	// the cap — not 30 chosen by updated_at out of the wrong 70.
	if int(srv.changeCalls.Load()) != 30 {
		t.Errorf("made %d /changes calls, want exactly the cap of 30", srv.changeCalls.Load())
	}
}

// TestKeepNewestMRs_AgreesWithCapEvents: keepNewestMRs trims the list
// before paying for a /changes call per MR, and prompt.Render then runs
// prompt.capEvents over everything and keeps the newest MaxEvents. The
// trim's stated purpose is that "an MR capEvents would drop anyway" must
// not cost a request — which only holds if the two order identically.
//
// They did not. keepNewestMRs tied on numeric IID; capEvents ties on
// Kind+Ref, and for MRs that is "#10" < "#9". So the trim kept MR 9
// while the rendered prompt would have kept MR 10: the request was spent
// on nothing and MR 10 rendered with no "+N/-M in K files" suffix.
//
// Driven through prompt.Render rather than capEvents directly, because
// capEvents is unexported and going through the real renderer is the
// stronger claim anyway: the rendered prompt and the fetched MR must
// name the same merge request.
func TestKeepNewestMRs_AgreesWithCapEvents(t *testing.T) {
	// IIDs chosen so numeric and string order disagree: 9 vs 10.
	for _, iids := range [][]int{{9, 10}, {10, 9}, {9, 10, 100, 2}} {
		at := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)
		mrs := make([]gitlab.MergeRequest, 0, len(iids))
		evs := make([]events.NormalizedEvent, 0, len(iids))
		for _, iid := range iids {
			m := gitlab.MergeRequest{IID: iid, Title: "work", UpdatedAt: at}
			m.MergedAt = &at
			mrs = append(mrs, m)
			evs = append(evs, events.MR(m))
		}

		kept, _ := keepNewestMRs(mrs, 1)
		_, user, err := prompt.Render(prompt.AudienceTeam, "group/p", evs, 1, nil, prompt.Hint{})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		rendered := "#" + strconv.Itoa(kept[0].IID)
		t.Logf("iids %v -> fetched /changes for #%d; the prompt kept %s",
			iids, kept[0].IID, onlyRef(user))
		if !strings.Contains(user, rendered) {
			t.Errorf("iids %v: fetched /changes for %s but the prompt did not keep "+
				"it (it kept %s) — the request was spent on an MR the prompt discards",
				iids, rendered, onlyRef(user))
		}
	}
}

// onlyRef returns the single "#N" reference in a rendered prompt, or a
// marker if there is not exactly one, so the failure message says what
// actually survived rather than repeating the input.
func onlyRef(promptText string) string {
	var found []string
	for _, f := range strings.Fields(promptText) {
		f = strings.TrimLeft(f, "#")
		end := strings.IndexFunc(f, func(r rune) bool { return r < '0' || r > '9' })
		if end > 0 {
			found = append(found, "#"+f[:end])
		}
	}
	return strings.Join(found, ",")
}

// TestKeepNewestMRs_TieBreakPrefersTheRefOrder: pinned directly, so the
// agreement test above is not the only thing standing between a future
// edit and the two functions drifting apart again.
func TestKeepNewestMRs_TieBreakPrefersTheRefOrder(t *testing.T) {
	at := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)
	mk := func(iid int) gitlab.MergeRequest {
		m := gitlab.MergeRequest{IID: iid, Title: "m", UpdatedAt: at}
		m.MergedAt = &at
		return m
	}
	// "#10" sorts before "#9" as a string, so MR 10 is the one the cap
	// would keep — the opposite of "lower IID wins".
	kept, dropped := keepNewestMRs([]gitlab.MergeRequest{mk(9), mk(10)}, 1)
	if kept[0].IID != 10 || dropped != 1 {
		t.Errorf("kept IID %d (dropped %d), want 10 — must match capEvents' "+
			"string order on #10 < #9", kept[0].IID, dropped)
	}
}
