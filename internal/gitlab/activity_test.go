package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// activityServer says \"active\" or not per project, with separate
// overrides for the MR probe and the issue probe. The probe endpoint
// returns 1 item (active) or [] (inactive) so the function's `len > 0`
// check does the rest.
type activityServer struct {
	mrByPath    map[string]bool // project path → MR active?
	issueByPath map[string]bool // project path → issue active?
	mrErr       int             // status to return for the MR endpoint (0 = ok)
	issueErr    int             // status to return for the issue endpoint
}

func (a *activityServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is URL-decoded, so project paths with slashes
		// come through naturally. The kind ("merge_requests" /
		// "issues") is the last URL segment, so we can match the
		// suffix without having to know how many slashes the project
		// path has.
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		var projPath, kind string
		switch {
		case strings.HasSuffix(path, "/merge_requests"):
			projPath, kind = strings.TrimSuffix(path, "/merge_requests"), "merge_requests"
		case strings.HasSuffix(path, "/issues"):
			projPath, kind = strings.TrimSuffix(path, "/issues"), "issues"
		default:
			http.Error(w, "unhandled subpath "+path, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("per_page") != "1" {
			// The probe is always per_page=1; the full fetch uses
			// per_page=100 and is exercised by other tests.
			http.Error(w, "unexpected per_page", http.StatusInternalServerError)
			return
		}
		var activeMap map[string]bool
		var forcedStatus int
		switch kind {
		case "merge_requests":
			activeMap = a.mrByPath
			forcedStatus = a.mrErr
		case "issues":
			activeMap = a.issueByPath
			forcedStatus = a.issueErr
		}
		w.Header().Set("Content-Type", "application/json")
		if forcedStatus != 0 {
			w.WriteHeader(forcedStatus)
			_, _ = w.Write([]byte(`{"message":"forced"}`))
			return
		}
		if activeMap[projPath] {
			_, _ = w.Write([]byte(`[{"iid":1}]`))
		} else {
			_, _ = w.Write([]byte(`[]`))
		}
	})
	return mux
}

// TestProbeProjectActivity_MRsCount: the original behaviour. Projects
// with at least one MR in the window are active.
func TestProbeProjectActivity_MRsCount(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/p": true},
		issueByPath: map[string]bool{"g/p": false},
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !active {
		t.Error("project with MRs should be active")
	}
}

// TestProbeProjectActivity_IssuesCount: a project whose only activity
// is issue updates (no MRs) is active. This is the headline change.
func TestProbeProjectActivity_IssuesCount(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/p": false},
		issueByPath: map[string]bool{"g/p": true},
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !active {
		t.Error("project with only issues in window should be active — that's the whole point of the widening")
	}
}

// TestProbeProjectActivity_NeitherStillInactive: a project with neither
// MRs nor issues in the window is not active.
func TestProbeProjectActivity_NeitherStillInactive(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/p": false},
		issueByPath: map[string]bool{"g/p": false},
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if active {
		t.Error("project with no MRs and no issues should not be active")
	}
}

// TestProbeProjectActivity_404SoftDegrades: a 404 on either endpoint
// degrades to (false, nil). The project might be confidential or
// have been moved; we don't want the discovery loop to error out on
// every unreachable project.
func TestProbeProjectActivity_404SoftDegrades(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/gone": false},
		issueByPath: map[string]bool{"g/gone": false},
		mrErr:       http.StatusNotFound,
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/gone", time.Now())
	if err != nil {
		t.Fatalf("404 should NOT error: %v", err)
	}
	if active {
		t.Error("missing project must not appear active")
	}
}

// TestProbeProjectActivity_Issue404AlsoSoftDegrades: a 404 on the
// issue endpoint (not the MR endpoint) must also degrade silently.
func TestProbeProjectActivity_Issue404AlsoSoftDegrades(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/issue404": true},
		issueByPath: map[string]bool{"g/issue404": false},
		issueErr:    http.StatusNotFound,
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	// MR returns active, so the project IS active even with the issue
	// 404. The probe doesn't need to wait for both to succeed; first
	// active wins.
	active, err := c.ProbeProjectActivity(context.Background(), "g/issue404", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !active {
		t.Error("MR-active project should be active even when the issue endpoint 404s")
	}
}

// TestProbeProjectActivity_403HardErrors: 403 is a real auth/permissions
// problem on the MR endpoint and must hard-fail. Soft-degrading would
// silently hide a project from the report.
func TestProbeProjectActivity_403HardErrors(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/private": false},
		issueByPath: map[string]bool{"g/private": false},
		mrErr:       http.StatusForbidden,
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	_, err := c.ProbeProjectActivity(context.Background(), "g/private", time.Now())
	if err == nil {
		t.Fatal("403 should hard-error")
	}
	if err != nil && !strings.Contains(err.Error(), "403") {
		t.Errorf("403 error should mention status code: %v", err)
	}
}

// TestProbeProjectActivity_429Surfaces: a 429 is a real rate-limit
// problem; surface it as an error rather than silently dropping the
// project.
func TestProbeProjectActivity_429Surfaces(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:    map[string]bool{"g/rate": false},
		issueByPath: map[string]bool{"g/rate": false},
		mrErr:       http.StatusTooManyRequests,
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	_, err := c.ProbeProjectActivity(context.Background(), "g/rate", time.Now())
	if err == nil {
		t.Fatal("429 should surface an error so the operator notices")
	}
}
