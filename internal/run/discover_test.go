package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// fakeGitLabForDiscover returns a *gitlab.Client backed by an httptest
// server that records the request sequence and replies with canned
// project lists / MR probes. Counts let each test assert on what
// discovery did. The fake here is intentionally minimal — only the
// endpoints the discover path exercises.
type fakeGitLabForDiscover struct {
	groupRequests atomic.Int64
	mrRequests    atomic.Int64

	// index keyed by project path; each entry is a static HTTP handler
	// the test wants for /projects/:path/merge_requests.
	mrHandlers map[string]http.HandlerFunc
}

func (f *fakeGitLabForDiscover) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects") {
			http.Error(w, "unexpected group path", http.StatusNotFound)
			return
		}
		f.groupRequests.Add(1)
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 1, "path_with_namespace": "infra/payments", "last_activity_at": "2026-10-19T00:00:00Z"}]`))
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is URL-decoded by net/http; project paths
		// contain slashes (`infra/payments`) so the decoded form
		// has multiple slashes. Use EscapedPath so the matching
		// prefix is the encoded form consistent with the prefix
		// registration above.
		const prefix = "/api/v4/projects/"
		rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
		parts := strings.Split(rest, "/")
		if len(parts) < 2 || parts[1] != "merge_requests" {
			http.Error(w, "unexpected path: "+r.URL.EscapedPath(), http.StatusNotFound)
			return
		}
		projPath, err := urlPathUnescape(parts[0])
		if err != nil {
			http.Error(w, "bad encoding", http.StatusBadRequest)
			return
		}
		f.mrRequests.Add(1)
		if h, ok := f.mrHandlers[projPath]; ok {
			h(w, r)
			return
		}
		// Default: empty MR list (= inactive).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	return mux
}

// urlPathUnescape reverses url.PathEscape so the test can match a
// GitLab URL path against the original project path string.
func urlPathUnescape(s string) (string, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out), nil
}

func unhex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

func newFakeServer(t *testing.T, f *fakeGitLabForDiscover) (string, *gitlab.Client) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := gitlab.New(srv.URL, "glpat-test", 5*time.Second)
	return srv.URL, c
}

// TestResolveProjects_OnlyExplicit: no --discover-group means no
// group fetch happens and the explicit list passes through unchanged.
func TestResolveProjects_OnlyExplicit(t *testing.T) {
	f := &fakeGitLabForDiscover{mrHandlers: map[string]http.HandlerFunc{}}
	_, gc := newFakeServer(t, f)

	proj, disc, _, err := resolveProjects(context.Background(), gc, Options{
		Projects: []string{"group/a", "group/b"},
		Since:    7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(proj) != 2 || proj[0] != "group/a" || proj[1] != "group/b" {
		t.Errorf("unexpected explicit project list: %v", proj)
	}
	if len(disc) != 0 {
		t.Errorf("no --discover-group should mean no discovered: %v", disc)
	}
	if f.groupRequests.Load() != 0 {
		t.Errorf("no group fetch expected when not discovering")
	}
}

// TestResolveProjects_DiscoverOnly: --discover-group fetches the
// group's projects, probes each one with an MR list call, and
// includes only the active ones in the final list.
func TestResolveProjects_DiscoverOnly(t *testing.T) {
	f := &fakeGitLabForDiscover{mrHandlers: map[string]http.HandlerFunc{
		"infra/payments": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"iid": 1, "title": "active"}]`))
		},
		"infra/quiet": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/projects") {
			f.groupRequests.Add(1)
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id": 1, "path_with_namespace": "infra/payments", "last_activity_at": "2026-10-19T00:00:00Z"},
				{"id": 2, "path_with_namespace": "infra/quiet",    "last_activity_at": "2026-10-15T00:00:00Z"}
			]`))
			return
		}
		f.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	proj, disc, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoveredGroups: []string{"infra"},
		Since:            7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(proj) != 1 || proj[0] != "infra/payments" {
		t.Errorf("only the MR-active project should be in final list: %v", proj)
	}
	if !disc["infra/payments"] {
		t.Errorf("discovered project should be tagged: %+v", disc)
	}
	if f.mrRequests.Load() != 2 {
		t.Errorf("expected 2 probe calls (one per project), got %d", f.mrRequests.Load())
	}
}

// TestResolveProjects_ComposableDedupes: --project and --discover-group
// together. Explicit projects pass through; the discovery pass adds
// new ones but skips projects that were already explicit-listed.
// An explicit project path stays out of the discovered set even if
// it appeared in the group's project list.
func TestResolveProjects_ComposableDedupes(t *testing.T) {
	f := &fakeGitLabForDiscover{mrHandlers: map[string]http.HandlerFunc{
		"infra/discovered-only": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"iid": 7, "title": "active discovered"}]`))
		},
		// overlap would, if probed, return MRs. We must NOT probe
		// it because it's already in the explicit list — the
		// discovery pass should skip-and-continue.
		"infra/overlap": func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("explicit-listed overlap should NOT be probed")
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/projects") {
			f.groupRequests.Add(1)
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id": 1, "path_with_namespace": "infra/overlap",          "last_activity_at": "2026-10-19T00:00:00Z"},
				{"id": 2, "path_with_namespace": "infra/discovered-only",  "last_activity_at": "2026-10-19T00:00:00Z"}
			]`))
			return
		}
		f.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	gc := gitlab.New(srv.URL, "glpat-test", 5*time.Second)

	proj, disc, _, err := resolveProjects(context.Background(), gc, Options{
		Projects:         []string{"infra/overlap"},
		DiscoveredGroups: []string{"infra"},
		Since:            7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(proj) != 2 || proj[0] != "infra/overlap" || proj[1] != "infra/discovered-only" {
		t.Errorf("expected [overlap, discovered-only], got %v", proj)
	}
	if disc["infra/overlap"] {
		t.Errorf("explicit overlap should not be marked as discovered: %+v", disc)
	}
	if !disc["infra/discovered-only"] {
		t.Errorf("discovered-only should be marked as discovered: %+v", disc)
	}
	// Probe should fire only once — for the discovered-only project.
	// The test's `infra/overlap` handler will t.Fatal if invoked.
	if got := f.mrRequests.Load(); got != 1 {
		t.Errorf("expected exactly 1 probe call (skipping explicit), got %d", got)
	}
}
