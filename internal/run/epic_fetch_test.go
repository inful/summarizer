package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestFetchProjectEpics_MissingUpdatedAtKeepsHighestIIDs: the recency
// sort is what stops the cap from returning the group's oldest epics.
// If the API omits updated_at — an older self-hosted GitLab, a
// permissions-filtered response — every timestamp is the zero time,
// every comparison ties, and the IID tiebreak hands back the OLDEST
// epics. That silently reintroduces the exact bug the sort exists to
// prevent, while the log still claims "most recently updated". The
// fixture below sends no updated_at at all.
func TestFetchProjectEpics_MissingUpdatedAtKeepsHighestIIDs(t *testing.T) {
	p, _ := newEpicPipeline(t, 60)

	epics, err := p.fetchProjectEpics(context.Background(), "g/p", time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("fetchProjectEpics: %v", err)
	}
	if len(epics) != maxActiveEpics {
		t.Fatalf("returned %d epics, want the cap of %d", len(epics), maxActiveEpics)
	}
	// Highest IIDs first in the fixture, so the newest are epics 41..60.
	wantLow := 60 - maxActiveEpics + 1
	if epics[0].IID != wantLow {
		t.Errorf("kept epics starting at IID %d, want %d; "+
			"without updated_at the highest IIDs are the only newer-by-construction signal",
			epics[0].IID, wantLow)
	}
	if epics[len(epics)-1].IID != 60 {
		t.Errorf("last epic is IID %d, want 60", epics[len(epics)-1].IID)
	}
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
// a real content change. The rendered order is by IID regardless of
// which epics the cap selected.
func TestFetchProjectEpics_SortedByIID(t *testing.T) {
	p, _ := newEpicPipeline(t, 5)

	first, err := p.fetchProjectEpics(context.Background(), "g/p", time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 2 {
		t.Skip("need at least 2 epics to assert ordering")
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].IID > first[i].IID {
			t.Fatalf("epics not sorted by IID: %d before %d", first[i-1].IID, first[i].IID)
		}
	}
}

// recencyEpicServer gives each epic an updated_at that DECREASES with
// IID, so the 10 highest-IID epics are the most recently touched. That
// is the shape that breaks an IID-ordered cap.
type recencyEpicServer struct {
	epics       int
	issueCalls  int
	updatedBase int // days ago for the highest-IID epic
}

func (e *recencyEpicServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/epics/") && strings.HasSuffix(p, "/issues"):
			e.issueCalls++
			// The issue's activity mirrors the epic's own recency, so
			// the high-IID epics are the active ones. The activity
			// filter reads the ISSUE timestamp, not the epic's, so this
			// is what decides whether an epic survives.
			iid := epicIIDFromPath(p)
			daysAgo := e.updatedBase - (iid - 1)
			stamp := time.Now().AddDate(0, 0, -daysAgo).UTC().Format(time.RFC3339)
			_, _ = w.Write([]byte(`[{"iid":1,"updated_at":"` + stamp + `"}]`))
		case strings.HasSuffix(p, "/epics"):
			var list []map[string]any
			for i := 1; i <= e.epics; i++ {
				// Higher IID = more recently updated.
				daysAgo := e.updatedBase - (i - 1)
				list = append(list, map[string]any{
					"iid": i, "title": fmt.Sprintf("Epic %02d", i), "state": "opened",
					"labels":     []string{},
					"updated_at": time.Now().AddDate(0, 0, -daysAgo).UTC().Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[],"namespace":{"kind":"group","full_path":"g"}}`))
	})
	return mux
}

// epicIIDFromPath pulls the epic iid out of
// /api/v4/groups/g/epics/<iid>/issues.
func epicIIDFromPath(p string) int {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg == "epics" && i+1 < len(parts) {
			n, _ := strconv.Atoi(parts[i+1])
			return n
		}
	}
	return 0
}

func newEpicPipelineFrom(t *testing.T, s *recencyEpicServer) *Pipeline {
	t.Helper()
	hs := httptest.NewServer(s.handler())
	t.Cleanup(hs.Close)
	return &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(hs.URL, "glpat-x", 5*time.Second),
		LLM:       &failingLLM{},
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
		Opts:      Options{Since: 168 * time.Hour, DryRun: true},
	}
}

// TestFetchProjectEpics_CapKeepsRecentlyUpdatedNotLowestIID: the cap
// decides which epics get their issues fetched, so slicing by IID
// picked the 20 OLDEST epics and then filtered those by activity. A
// group whose recent epics all have high IIDs therefore reported zero
// epics while its stale ones were the only ones ever considered.
// Selection must be by recency, which costs nothing — updated_at is
// already in the /epics list response.
func TestFetchProjectEpics_CapKeepsRecentlyUpdatedNotLowestIID(t *testing.T) {
	// updatedBase 25: iid 20 is 6 days old (inside the 7d window) and
	// iid 1 is 25 days old (outside) — so epics 14-30 are active and
	// 1-13 are dormant. An IID-ordered cap takes 1-20 and would report
	// only epics 14-20, silently dropping 21-30.
	srv := &recencyEpicServer{epics: 30, updatedBase: 25}
	p := newEpicPipelineFrom(t, srv)

	epics, err := p.fetchProjectEpics(context.Background(), "g/p", time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("fetchProjectEpics: %v", err)
	}
	// Epics with IID >= 20 are the active ones in this fixture
	// (updatedBase 25 puts the 7-day activity boundary at IID 20).
	// An IID-ordered cap fetches IIDs 1-20 and can only ever report
	// epic 20 — it never sees the other 10 active epics at all.
	var recent int
	for _, e := range epics {
		if e.IID >= 20 {
			recent++
		}
	}
	if recent < 2 {
		t.Fatalf("only %d of the 11 active epics reported (ids=%v): the cap took the "+
			"20 lowest IIDs and discarded every active epic above IID %d before it could "+
			"be checked. issueCalls=%d",
			recent, epicIDs(epics), maxActiveEpics, srv.issueCalls)
	}
	if srv.issueCalls > maxActiveEpics {
		t.Errorf("made %d issue calls, over the cap of %d", srv.issueCalls, maxActiveEpics)
	}
	// Rendered order stays by IID even though selection was by recency.
	for i := 1; i < len(epics); i++ {
		if epics[i-1].IID > epics[i].IID {
			t.Errorf("rendered order not by IID: %d before %d", epics[i-1].IID, epics[i].IID)
		}
	}
}

func epicIDs(eps []*gitlab.Epic) []int {
	out := make([]int, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.IID)
	}
	return out
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
