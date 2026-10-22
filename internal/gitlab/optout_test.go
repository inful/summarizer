package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// optOutServer is a fake GitLab that answers both /projects/:id (for
// the DefaultBranch lookup) and /repository/files/:name. optOutByPath
// maps a project path to a list of sentinel file names that should
// be reported as present; any other file on that project is 404.
type optOutServer struct {
	optOutByPath  map[string][]string
	defaultBranch string
	fileHits      int
}

func (o *optOutServer) handler() http.Handler {
	mux := http.NewServeMux()
	// Two distinct path patterns: the project-metadata endpoint and
	// the file endpoint. /api/v4/projects/ and /api/v4/projects/g%2F/
	// would collide as separate mux patterns because the second is
	// a more-specific case of the first. Handle them by prefix
	// matching in a single handler.
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		// The path segment after /repository/files/ is the file name.
		// percent-decoded by net/http so we can match on the literal.
		if strings.Contains(r.URL.EscapedPath(), "/repository/files/") {
			fileName := strings.TrimPrefix(r.URL.EscapedPath(),
				"/api/v4/projects/g%2Fp/repository/files/")
			if decoded, err := url.PathUnescape(fileName); err == nil {
				fileName = decoded
			}
			o.fileHits++
			for _, want := range o.optOutByPath["g/p"] {
				if fileName == want {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"file_name":"` + want + `","file_path":"` + want + `","size":0,"encoding":"text","ref":"` + o.defaultBranch + `","commit_id":"abc","last_commit_id":"abc"}`))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Project metadata: return the default branch for the
		// GetDefaultBranch call.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"path_with_namespace":"g/p","last_activity_at":"2026-10-19T00:00:00Z","topics":[],"default_branch":"` + o.defaultBranch + `"}`))
	})
	return mux
}

// urlPathUnescape helper removed — using net/url's PathUnescape directly.

// TestHasOptOutFile_DotSummaryIgnore: the summariser-specific opt-out
// file. When present at repo root, the project is excluded from the
// run.
func TestHasOptOutFile_DotSummaryIgnore(t *testing.T) {
	srv := httptest.NewServer((&optOutServer{
		optOutByPath:  map[string][]string{"g/p": {".summaryignore"}},
		defaultBranch: "main",
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, err := c.HasOptOutFile(context.Background(), "g/p", "main")
	if err != nil {
		t.Fatalf("HasOptOutFile: %v", err)
	}
	if !opted {
		t.Error("project with .summaryignore should be opted out")
	}
}

// TestHasOptOutFile_DotDocIgnore: the docbuilder-convention opt-out
// alias. Same semantics as .summaryignore — either file opts out.
func TestHasOptOutFile_DotDocIgnore(t *testing.T) {
	srv := httptest.NewServer((&optOutServer{
		optOutByPath:  map[string][]string{"g/p": {".docignore"}},
		defaultBranch: "main",
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, err := c.HasOptOutFile(context.Background(), "g/p", "main")
	if err != nil {
		t.Fatalf("HasOptOutFile: %v", err)
	}
	if !opted {
		t.Error("project with .docignore should be opted out")
	}
}

// TestHasOptOutFile_NotPresent: when neither file exists at the repo
// root, the project is NOT opted out — it goes through the rest of
// the audience pipeline.
func TestHasOptOutFile_NotPresent(t *testing.T) {
	srv := httptest.NewServer((&optOutServer{
		optOutByPath:  map[string][]string{"g/p": {}},
		defaultBranch: "main",
	}).handler())
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, err := c.HasOptOutFile(context.Background(), "g/p", "main")
	if err != nil {
		t.Fatalf("HasOptOutFile: %v", err)
	}
	if opted {
		t.Error("project with no opt-out file should NOT be opted out")
	}
}

// TestHasOptOutFile_ProjectNotFound: a moved/renamed project returns
// (false, nil) so the discovery loop doesn't fail the run on
// transient metadata misses — same contract as FetchProjectSummary.
func TestHasOptOutFile_ProjectNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.EscapedPath(), "/api/v4/projects/") {
			http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, err := c.HasOptOutFile(context.Background(), "g/missing", "main")
	if err != nil {
		t.Fatalf("404 should NOT error: %v", err)
	}
	if opted {
		t.Error("missing project must not appear opted out")
	}
}

// TestHasOptOutFile_403DegradesToNotFound: a PAT that can read metadata
// but not the file (e.g. confidential projects) returns (false, nil).
// The project-owner signal can't reach us, so we treat the project as
// opted-in. The run continues; if the file truly exists the operator
// will see the published report and can iterate.
func TestHasOptOutFile_403DegradesToNotFound(t *testing.T) {
	var fileHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.EscapedPath(), "/api/v4/projects/") &&
			strings.Contains(r.URL.EscapedPath(), "/repository/files/"):
			fileHits++
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main","last_activity_at":"2026-10-19T00:00:00Z"}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, err := c.HasOptOutFile(context.Background(), "g/private", "main")
	if err != nil {
		t.Fatalf("403 should NOT error: %v", err)
	}
	if opted {
		t.Error("403 must not appear opted out — treat as not-found")
	}
	if fileHits == 0 {
		t.Error("expected at least one file request")
	}
}

// TestHasOptOutFile_OnlyHitsFilesEndpointOnceForFirstMatch: the
// summariser-specific .summaryignore is checked first; the
// docbuilder alias is only checked when the first file is absent.
// Verifies the short-circuit ordering.
func TestHasOptOutFile_OnlyHitsFilesEndpointOnceForFirstMatch(t *testing.T) {
	var fileHits int
	srv := httptest.NewServer((&optOutServer{
		optOutByPath:  map[string][]string{"g/p": {".summaryignore"}},
		defaultBranch: "main",
	}).handler())
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.EscapedPath(), "/repository/files/") {
			fileHits++
		}
		// Delegate to the real handler.
		(&optOutServer{
			optOutByPath:  map[string][]string{"g/p": {".summaryignore"}},
			defaultBranch: "main",
		}).handler().ServeHTTP(w, r)
	})
	defer srv.Close()
	c := New(srv.URL, "glpat-test", 5*time.Second)
	opted, _ := c.HasOptOutFile(context.Background(), "g/p", "main")
	if !opted {
		t.Fatal("expected opted out")
	}
	// Should have hit the files endpoint exactly once (for .summaryignore)
	// — .docignore short-circuited away.
	if fileHits != 1 {
		t.Errorf("want 1 file hit (short-circuit on .summaryignore), got %d", fileHits)
	}
}
