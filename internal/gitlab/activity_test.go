package gitlab

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
)

// activityServer says \"active\" or not per project, with separate
// overrides for the MR probe and the issue probe. The probe endpoint
// returns 1 item (active) or [] (inactive) so the function's `len > 0`
// check does the rest.
type activityServer struct {
	mrByPath     map[string]bool // project path → MR active?
	issueByPath  map[string]bool // project path → issue active?
	commitByPath map[string]bool // project path → commit active?
	mrErr        int             // status to return for the MR endpoint (0 = ok)
	issueErr     int             // status to return for the issue endpoint
	commitProbes int             // counts how often the commit probe is reached
}

func (a *activityServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is URL-decoded, so project paths with slashes
		// come through naturally. The kind ("merge_requests" /
		// "issues" / "repository/commits") is the last URL segment,
		// so we can match the suffix without having to know how many
		// slashes the project path has.
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		var projPath, kind string
		switch {
		case strings.HasSuffix(path, "/merge_requests"):
			projPath, kind = strings.TrimSuffix(path, "/merge_requests"), "merge_requests"
		case strings.HasSuffix(path, "/issues"):
			projPath, kind = strings.TrimSuffix(path, "/issues"), "issues"
		case strings.HasSuffix(path, "/repository/commits"):
			projPath, kind = strings.TrimSuffix(path, "/repository/commits"), "repository/commits"
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
		case "repository/commits":
			activeMap = a.commitByPath
			a.commitProbes++
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

// TestProbeProjectActivity_CommitsCount: a repo maintained by direct
// pushes to the default branch produces neither an MR nor an issue.
// Without the commit probe the whole week's work on such a repo is
// invisible, and under activity-default discovery those are exactly
// the repos most likely to be discovered.
func TestProbeProjectActivity_CommitsCount(t *testing.T) {
	srv := httptest.NewServer((&activityServer{
		mrByPath:     map[string]bool{"g/p": false},
		issueByPath:  map[string]bool{"g/p": false},
		commitByPath: map[string]bool{"g/p": true},
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	active, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !active {
		t.Error("project with only commits in window should be active")
	}
}

// TestProbeProjectActivity_CommitProbeIsLast: the MR probe answers most
// of the time, so the commit probe must not run once an MR hit lands.
// Pins the short-circuit order so a future reordering doesn't quietly
// add a round-trip to every active project.
func TestProbeProjectActivity_CommitProbeIsLast(t *testing.T) {
	as := &activityServer{
		mrByPath:     map[string]bool{"g/p": true},
		issueByPath:  map[string]bool{"g/p": false},
		commitByPath: map[string]bool{"g/p": true},
	}
	srv := httptest.NewServer(as.handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)

	if _, err := c.ProbeProjectActivity(context.Background(), "g/p", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if as.commitProbes != 0 {
		t.Errorf("commit probe ran %d times despite an MR hit; want 0", as.commitProbes)
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

// TestFetchActiveProjects_StopsAtPageCeiling: the endpoint has no
// server-side limit, so on an instance where the token can see
// thousands of projects the old code walked every page and held every
// ProjectSummary in memory — to then rank them and keep at most
// maxProjects. The scan must stop at a fixed number of pages and say
// that it did, so a truncated scan is never presented as complete.
func TestFetchActiveProjects_StopsAtPageCeiling(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		// Always report a next page, forever.
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		list := make([]map[string]any, 0, 100)
		for i := 0; i < 100; i++ {
			list = append(list, map[string]any{
				"id": page*100 + i + 1, "path_with_namespace": fmt.Sprintf("g/p%d", page*100+i+1),
			})
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer srv.Close()

	projs, err := New(srv.URL, "glpat-x", 5*time.Second).
		FetchActiveProjects(context.Background(), time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("FetchActiveProjects: %v", err)
	}
	if pages > maxActiveScanPages {
		t.Errorf("walked %d pages, want at most %d", pages, maxActiveScanPages)
	}
	if want := maxActiveScanPages * 100; len(projs) != want {
		t.Errorf("got %d projects, want %d (%d pages of 100)",
			len(projs), want, maxActiveScanPages)
	}
}

// TestHasOptOutFile_MakesNoProjectRequest: the method used to issue
// its own GET /projects/:id to resolve the default branch — a field
// both of its callers already held, because they reach it from a
// discovery scan or an explicit listing whose payload carries
// default_branch. On a 50-project capped run that was 50 requests
// spent re-reading what was in hand, before any per-project work.
//
// The ref is now a parameter, so the method talks only to the Files
// API.
func TestHasOptOutFile_MakesNoProjectRequest(t *testing.T) {
	var projectFetches, fileReads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/files/"):
			fileReads++
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(p, "/api/v4/projects/g/p"):
			projectFetches++
			_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Neither sentinel file exists, so both are probed.
	got, err := New(srv.URL, "glpat-x", 5*time.Second).HasOptOutFile(context.Background(), "g/p", "main")
	if err != nil {
		t.Fatalf("HasOptOutFile: %v", err)
	}
	if got {
		t.Error("no opt-out file exists; want false")
	}
	if projectFetches != 0 {
		t.Errorf("re-fetched the project summary %d times; the caller already had it", projectFetches)
	}
	if fileReads != len(optOutFileNames) {
		t.Errorf("file reads = %d, want %d (one per sentinel name)", fileReads, len(optOutFileNames))
	}
}

// TestHasOptOutFile_EmptyRefResolvesItRatherThanSkippingTheCheck: a
// caller with no default_branch is either an older GitLab whose
// /projects listing omits the field, or a moved/renamed project. The
// first must still get the opt-out check — dropping it would publish a
// report a project explicitly asked not to be published, which is
// worse than the request it saves. The second has nothing to read and
// is included by default.
func TestHasOptOutFile_EmptyRefResolvesItRatherThanSkippingTheCheck(t *testing.T) {
	t.Run("listing omits default_branch, single fetch resolves it", func(t *testing.T) {
		var projectFetches, fileReads int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "/repository/files/"):
				fileReads++
				_, _ = w.Write([]byte(`{"file_name":".summaryignore","file_path":".summaryignore"}`))
			default:
				projectFetches++
				_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","topics":[]}`))
			}
		}))
		defer srv.Close()

		got, err := New(srv.URL, "glpat-x", 5*time.Second).HasOptOutFile(context.Background(), "g/p", "")
		if err != nil {
			t.Fatalf("HasOptOutFile: %v", err)
		}
		if !got {
			t.Error("the opt-out file exists; the project must be excluded")
		}
		if projectFetches != 1 {
			t.Errorf("expected exactly one fallback project fetch, got %d", projectFetches)
		}
	})

	t.Run("project is gone, included by default", func(t *testing.T) {
		var fileReads int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/repository/files/") {
				fileReads++
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		got, err := New(srv.URL, "glpat-x", 5*time.Second).HasOptOutFile(context.Background(), "g/moved", "")
		if err != nil {
			t.Fatalf("a moved project must not error: %v", err)
		}
		if got {
			t.Error("a project that cannot be read must not appear opted out")
		}
		if fileReads != 0 {
			t.Errorf("made %d file reads with no resolvable ref", fileReads)
		}
	})
}
