package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/gitlab"
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
