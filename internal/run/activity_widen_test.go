package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// TestDiscovery_ProjectActiveByIssuesAlone: the headline widening. A
// project whose only window activity is issues (no MRs) joins the
// run. Before this, such a project — say, a triage-only week or a
// planning sprint — was silently dropped from the report and from
// every per-customer epic file.
func TestDiscovery_ProjectActiveByIssuesAlone(t *testing.T) {
	mux := http.NewServeMux()
	// Discovery: one project, no topics.
	mux.HandleFunc("/api/v4/groups/group/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 1, "path_with_namespace": "group/triage",
			"last_activity_at": "2026-10-19T00:00:00Z"}]`))
	})
	// MR probe: empty (no MRs in window).
	mux.HandleFunc("/api/v4/projects/group%2Ftriage/merge_requests", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	// Issue probe: non-empty (the project is alive).
	mux.HandleFunc("/api/v4/projects/group%2Ftriage/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"iid": 1}]`))
	})
	// Endpoints the per-project fetch hits (empty for "triage-only").
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if strings.HasSuffix(path, "/merge_requests") || strings.HasSuffix(path, "/issues") {
			return
		}
		if strings.HasSuffix(path, "/repository/commits") || strings.HasSuffix(path, "/epics") {
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if strings.HasPrefix(path, "/api/v4/projects/group%2F") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespace":{"kind":"group","full_path":"group"},"topics":[]}`))
			return
		}
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	finalList, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoveredGroups: []string{"group"},
		Since:            7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(finalList) != 1 || finalList[0] != "group/triage" {
		t.Fatalf("issues-only project should be active; got %v", finalList)
	}
}

// TestDiscovery_ProjectInactiveWithoutMRsOrIssues: a project with
// neither is still dropped.
func TestDiscovery_ProjectInactiveWithoutMRsOrIssues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/group/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 1, "path_with_namespace": "group/quiet",
			"last_activity_at": "2026-09-04T00:00:00Z"}]`))
	})
	for _, kind := range []string{"merge_requests", "issues"} {
		kind := kind
		mux.HandleFunc("/api/v4/projects/group%2Fquiet/"+kind, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	finalList, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoveredGroups: []string{"group"},
		Since:            7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(finalList) != 0 {
		t.Fatalf("a project with no MRs and no issues should be excluded; got %v", finalList)
	}
}

// TestProbeHasAny_ShortCircuitsOnFirstActive: the MR probe is the
// cheap path; if it returns active, the issue probe should NOT be
// hit. This is a property test, not just a behavioural one: the
// second HTTP call would be a real cost on every active project.
func TestProbeHasAny_ShortCircuitsOnFirstActive(t *testing.T) {
	mrHits := 0
	issueHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			mrHits++
			_, _ = w.Write([]byte(`[{"iid":1}]`))
		case strings.HasSuffix(r.URL.Path, "/issues"):
			issueHits++
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !active {
		t.Error("expected active")
	}
	if mrHits != 1 {
		t.Errorf("MR probe should fire exactly once, fired %d", mrHits)
	}
	if issueHits != 0 {
		t.Errorf("issue probe should be skipped when MR is active, fired %d", issueHits)
	}
}
