package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// wideScanServer serves N active projects and counts how many were
// actually probed. It models an instance far wider than the cap.
type wideScanServer struct {
	n         int
	probes    int
	fileReads int
}

func (w *wideScanServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("X-Next-Page", "")
		rw.Header().Set("Content-Type", "application/json")
		var list []map[string]any
		for i := 0; i < w.n; i++ {
			// Older the higher the index, so ranking is meaningful.
			at := time.Now().AddDate(0, 0, -i).UTC().Format(time.RFC3339)
			list = append(list, map[string]any{
				"id": i + 1, "path_with_namespace": fmt.Sprintf("g/p%03d", i),
				"last_activity_at": at, "topics": []string{},
			})
		}
		_ = json.NewEncoder(rw).Encode(list)
	})
	mux.HandleFunc("/api/v4/projects/", func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		rw.Header().Set("X-Next-Page", "")
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/files/"):
			w.fileReads++
			rw.WriteHeader(http.StatusNotFound)
		case strings.Contains(p, "/merge_requests"):
			w.probes++
			_, _ = rw.Write([]byte(`[{"iid":1}]`))
		case strings.Contains(p, "/issues"):
			_, _ = rw.Write([]byte(`[]`))
		case strings.Contains(p, "/repository/commits"):
			_, _ = rw.Write([]byte(`[]`))
		default:
			_, _ = rw.Write([]byte(`{"id":1,"default_branch":"main","topics":[]}`))
		}
	})
	return mux
}

// TestDiscoverActive_CapsProbingNotJustLLMCalls: --max-projects used to
// bound only the LLM bill. Every active project was still probed —
// opt-out file read plus activity probe, ~6 sequential round-trips
// each — and the list was trimmed afterwards. On an instance with 200
// active projects that is ~1200 requests spent building a list that is
// immediately cut to 50. The scan must stop admitting once the cap is
// reached.
func TestDiscoverActive_CapsProbingNotJustLLMCalls(t *testing.T) {
	srv := &wideScanServer{n: 200}
	hs := httptest.NewServer(srv.handler())
	defer hs.Close()
	gc := gitlab.New(hs.URL, "glpat-x", 5*time.Second)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		MaxProjects:    10,
		Since:          7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(projects) != 10 {
		t.Fatalf("want 10 projects, got %d", len(projects))
	}
	if srv.probes > 10 {
		t.Errorf("probed %d projects for a cap of 10; the discarded ones cost real requests", srv.probes)
	}
	// Two sentinel filenames are probed per project (.summaryignore and
	// .docignore), so 10 projects is 20 reads.
	if srv.fileReads > 20 {
		t.Errorf("read %d opt-out files for 10 projects (expected 20); "+
			"projects past the cap were still being read", srv.fileReads)
	}
}

// TestDiscoverActive_KeepsTheMostRecentlyActive: the projects kept
// must be the most recently active ones, not whichever came back first
// from the API. The scan ranks by last_activity_at, which is free —
// it is already in the /projects payload.
func TestDiscoverActive_KeepsTheMostRecentlyActive(t *testing.T) {
	srv := &wideScanServer{n: 50}
	hs := httptest.NewServer(srv.handler())
	defer hs.Close()
	gc := gitlab.New(hs.URL, "glpat-x", 5*time.Second)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		MaxProjects:    5,
		Since:          7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"g/p000", "g/p001", "g/p002", "g/p003", "g/p004"}
	if len(projects) != len(want) {
		t.Fatalf("got %v, want %v", projects, want)
	}
	for i := range want {
		if projects[i] != want[i] {
			t.Errorf("project %d: got %q, want %q (the 5 most recently active)",
				i, projects[i], want[i])
		}
	}
}
