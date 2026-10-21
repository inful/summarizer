package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/safety"
)

// epicCountingServer serves a group with `epics` epics and counts how
// many per-epic issue fetches actually happen.
type epicCountingServer struct {
	epics      int
	issueCalls int
	epicCalls  int
}

func (e *epicCountingServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/epics/") && strings.HasSuffix(p, "/issues"):
			e.issueCalls++
			_, _ = w.Write([]byte(`[{"iid":1,"updated_at":"2026-10-20T00:00:00Z"}]`))
		case strings.HasSuffix(p, "/epics"):
			e.epicCalls++
			var list []map[string]any
			for i := 1; i <= e.epics; i++ {
				list = append(list, map[string]any{
					"iid": i, "title": "Epic " + string(rune('A'+i%26)),
					"state": "opened", "labels": []string{},
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(p, "/repository/commits"),
			strings.Contains(p, "/merge_requests"),
			strings.Contains(p, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		default:
			// Project payload; namespace is a nested object.
			_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[],"namespace":{"kind":"group","full_path":"g"}}`))
		}
	})
	return mux
}

func newEpicPipeline(t *testing.T, epics int) (*Pipeline, *epicCountingServer) {
	t.Helper()
	srv := &epicCountingServer{epics: epics}
	hs := httptest.NewServer(srv.handler())
	t.Cleanup(hs.Close)
	return &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(hs.URL, "glpat-x", 5*time.Second),
		LLM:       &failingLLM{},
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
		Opts:      Options{Since: 168 * time.Hour, DryRun: true},
	}, srv
}

// TestFetchProjectEpics_CapsBeforeFetchingIssues: the cap must be
// applied BEFORE the per-epic issue fetch, not after. The old order
// made one sequential HTTP call per epic in the group and then threw
// away everything past 20 — so a group with 120 epics cost 120
// round-trips to produce a report that only used 20 of them.
func TestFetchProjectEpics_CapsBeforeFetchingIssues(t *testing.T) {
	p, srv := newEpicPipeline(t, 60)

	epics, err := p.fetchProjectEpics(context.Background(), "g/p", time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("fetchProjectEpics: %v", err)
	}
	if srv.epicCalls != 1 {
		t.Errorf("expected exactly 1 epic-list call, got %d", srv.epicCalls)
	}
	if srv.issueCalls > maxActiveEpics {
		t.Errorf("made %d per-epic issue calls; the cap of %d must bound them", srv.issueCalls, maxActiveEpics)
	}
	if len(epics) > maxActiveEpics {
		t.Errorf("returned %d epics, over the cap of %d", len(epics), maxActiveEpics)
	}
}

// TestFetchProjectEpics_SortedByIID: GitLab does not promise a stable
// order for /epics. Two runs over identical input must produce an
// identical prompt, otherwise the fingerprint flags churn that is not
// a real content change.
func TestFetchProjectEpics_SortedByIID(t *testing.T) {
	p, _ := newEpicPipeline(t, 5)

	first, err := p.fetchProjectEpics(context.Background(), "g/p", time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].IID > first[i].IID {
			t.Fatalf("epics not sorted by IID: %d before %d", first[i-1].IID, first[i].IID)
		}
	}
}

// TestRun_DryRunSkipsEpicFetch: --dry-run is the preview an operator
// runs before paying for a real run. It must not make one HTTP call
// per epic to produce output it then throws away.
func TestRun_DryRunSkipsEpicFetch(t *testing.T) {
	p, srv := newEpicPipeline(t, 40)
	p.Opts.Projects = []string{"g/p"}

	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if srv.issueCalls != 0 {
		t.Errorf("--dry-run made %d per-epic issue calls; it should make none", srv.issueCalls)
	}
	if srv.epicCalls != 0 {
		t.Errorf("--dry-run made %d epic-list calls; it should make none", srv.epicCalls)
	}
}
