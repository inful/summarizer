package run

import (
	"context"
	"encoding/json"
	"fmt"
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

// changesServer serves a project with n merge requests and counts the
// /changes calls — one per MR, which is the cost this file is about.
type changesServer struct {
	n           int
	changeCalls atomic.Int64
	epicCalls   atomic.Int64
}

func (s *changesServer) handler() http.Handler {
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
			var list []map[string]any
			for i := 1; i <= s.n; i++ {
				list = append(list, map[string]any{
					"iid": i, "title": fmt.Sprintf("MR %d", i),
					"state": "merged", "updated_at": time.Now().Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.Contains(p, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(p, "/repository/commits"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[]}`))
		}
	})
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		s.epicCalls.Add(1)
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	return mux
}

func newChangesPipeline(t *testing.T, n, maxEvents int) (*Pipeline, *changesServer) {
	t.Helper()
	srv := &changesServer{n: n}
	hs := httptest.NewServer(srv.handler())
	t.Cleanup(hs.Close)
	return &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: maxEvents},
		GitLab:    gitlab.New(hs.URL, "glpat-x", 5*time.Second),
		LLM:       &failingLLM{},
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
		Opts:      Options{Since: weekStart(), Projects: []string{"g/p"}, MaxProjects: 10},
	}, srv
}

// TestRunOneProject_MaxEventsBoundsTheChangesFetch: capEvents keeps the
// newest MaxEvents events, so an MR past the cap was costing a full
// /merge_requests/:iid/changes request to be thrown away — and
// max_events is the flag an operator reaches for to bound exactly this.
// The bound has to sit before the fetch, not after the render.
func TestRunOneProject_MaxEventsBoundsTheChangesFetch(t *testing.T) {
	p, srv := newChangesPipeline(t, 40, 5)

	if _, err := p.runOneProject(context.Background(), "g/p"); err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if got := srv.changeCalls.Load(); got != 5 {
		t.Errorf("made %d /changes calls for a project with 40 MRs and --max-events 5; "+
			"the cap must bound the fetch, not just the prompt", got)
	}
}

// TestRunOneProject_DryRunMakesNoChangesCall: the preview reports
// counts, and the MR list already carries them. Paying one request per
// MR for output the run discards makes the preview slower than the run
// it previews, which is how you stop people previewing.
func TestRunOneProject_DryRunMakesNoChangesCall(t *testing.T) {
	p, srv := newChangesPipeline(t, 30, 200)
	p.Opts.DryRun = true

	res, err := p.runOneProject(context.Background(), "g/p")
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if got := srv.changeCalls.Load(); got != 0 {
		t.Errorf("dry-run made %d /changes call(s); it should make none", got)
	}
	if got := srv.epicCalls.Load(); got != 0 {
		t.Errorf("dry-run made %d epic call(s); it should make none", got)
	}
	if res.EventsFetched != 30 {
		t.Errorf("dry-run should still report the MR count, got %d", res.EventsFetched)
	}
}

// TestKeepNewestMRs: the trim and prompt.capEvents must agree on what
// "the newest n" means, or an event can survive the cap with no
// /changes data behind it.
func TestKeepNewestMRs(t *testing.T) {
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var mrs []gitlab.MergeRequest
	for i := 1; i <= 10; i++ {
		mrs = append(mrs, gitlab.MergeRequest{
			IID:       i,
			UpdatedAt: base.Add(-time.Duration(10-i) * time.Hour),
		})
	}
	kept, dropped := keepNewestMRs(mrs, 3)
	if dropped != 7 {
		t.Errorf("dropped = %d, want 7", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d, want 3", len(kept))
	}
	// IID 10 is the newest (updated 9h ago), 9 next, 8 next.
	for i, want := range []int{10, 9, 8} {
		if kept[i].IID != want {
			t.Errorf("kept[%d].IID = %d, want %d", i, kept[i].IID, want)
		}
	}
	// Under the cap: unchanged.
	kept, dropped = keepNewestMRs(mrs, 50)
	if dropped != 0 || len(kept) != 10 {
		t.Errorf("keepNewestMRs under the cap dropped something: %d dropped, %d kept", dropped, len(kept))
	}
}

func TestKeepNewestMRs_TiebreakIsDeterministic(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var mrs []gitlab.MergeRequest
	// Descending IID, identical timestamps: GitLab promises no order, so
	// two runs over identical input must produce identical documents.
	for i := 10; i >= 1; i-- {
		mrs = append(mrs, gitlab.MergeRequest{IID: i, UpdatedAt: at})
	}
	kept, _ := keepNewestMRs(mrs, 3)
	for i, want := range []int{1, 2, 3} {
		if kept[i].IID != want {
			t.Errorf("tie broken by order, not IID: kept[%d].IID = %d, want %d", i, kept[i].IID, want)
		}
	}
}
