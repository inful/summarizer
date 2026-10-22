package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/policy"
)

// optOutServer serves projects with per-path topics and an opt-out
// file, so a test can pin which signal excluded what.
type optOutServer struct {
	topics    map[string][]string
	files     map[string]bool // project path → has an opt-out file
	fileCalls map[string]int  // project path → file reads
	active    map[string]bool
}

func (o *optOutServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")

		// Decode the project path out of the escaped URL.
		proj := ""
		if i := strings.Index(p, "/projects/"); i >= 0 {
			rest := p[i+len("/projects/"):]
			rest = strings.TrimSuffix(rest, "/merge_requests")
			rest = strings.TrimSuffix(rest, "/issues")
			rest = strings.TrimSuffix(rest, "/repository/commits")
			rest = strings.TrimSuffix(rest, "/repository/files/.summaryignore")
			rest = strings.TrimSuffix(rest, "/repository/files/.docignore")
			proj = strings.ReplaceAll(rest, "%2F", "/")
		}

		switch {
		case strings.Contains(p, "/repository/files/"):
			if o.fileCalls != nil {
				o.fileCalls[proj]++
			}
			if o.files[proj] {
				_, _ = w.Write([]byte(`{"file_name":".summaryignore","file_path":".summaryignore"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)

		case strings.Contains(p, "/merge_requests"):
			if o.active[proj] {
				_, _ = w.Write([]byte(`[{"iid":1}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case strings.Contains(p, "/issues"):
			_, _ = w.Write([]byte(`[]`))

		case strings.HasSuffix(p, "/projects") && r.URL.Query().Get("last_activity_after") != "":
			var list []map[string]any
			for path, ts := range o.topics {
				list = append(list, map[string]any{
					"id": 1, "path_with_namespace": path,
					"last_activity_at": "2026-10-20T00:00:00Z", "topics": ts,
				})
			}
			jsonEncode(w, list)

		case strings.HasPrefix(p, "/api/v4/projects"):
			ts := o.topics[proj]
			jsonEncode(w, map[string]any{
				"id": 1, "path_with_namespace": proj, "default_branch": "main", "topics": ts,
			})

		case strings.HasPrefix(p, "/api/v4/groups/"):
			_, _ = w.Write([]byte(`[]`))

		default:
			jsonEncode(w, map[string]any{})
		}
	})
}

func jsonEncode(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func newOptOutClient(t *testing.T, o *optOutServer) *gitlab.Client {
	t.Helper()
	srv := httptest.NewServer(o.handler())
	t.Cleanup(srv.Close)
	return gitlab.New(srv.URL, "glpat-x", 5*time.Second)
}

// TestNoSummarizeTopic_ExcludesDiscoveredProject: the topic is the
// cheapest opt-out — the topics already arrived with the project
// summary, so it costs nothing and skips the file read entirely.
func TestNoSummarizeTopic_ExcludesDiscoveredProject(t *testing.T) {
	o := &optOutServer{
		topics: map[string][]string{
			"g/loud": {policy.TopicNoSummarize},
			"g/fine": {},
		},
		files:     map[string]bool{},
		fileCalls: map[string]int{},
		active:    map[string]bool{"g/loud": true, "g/fine": true},
	}
	gc := newOptOutClient(t, o)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, p := range projects {
		if p == "g/loud" {
			t.Fatalf("no-summarize project was not excluded: %v", projects)
		}
	}
	if len(projects) != 1 || projects[0] != "g/fine" {
		t.Errorf("want only g/fine, got %v", projects)
	}
	if o.fileCalls["g/loud"] != 0 {
		t.Errorf("the topic should short-circuit the file read, but made %d calls", o.fileCalls["g/loud"])
	}
}

// TestNoSummarizeTopic_WinsOverSummarize: an exclusion is an exclusion.
// A project carrying both `summarize` and `no-summarize` must not run,
// or the opt-in would silently beat the opt-out.
func TestNoSummarizeTopic_WinsOverSummarize(t *testing.T) {
	o := &optOutServer{
		topics: map[string][]string{
			"g/both": {policy.TopicSummarize, policy.TopicNoSummarize},
		},
		files:  map[string]bool{},
		active: map[string]bool{"g/both": true},
	}
	gc := newOptOutClient(t, o)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("no-summarize must beat summarize, got %v", projects)
	}
}

// TestOptOutOverridesExplicitProject: the operator chose "file wins"
// when the discovery model was inverted, but explicit --project entries
// were appended unconditionally and never reached the opt-out check —
// so the decision was never implemented. A project that asked not to be
// summarised must not be summarised just because a cron line names it.
func TestOptOutOverridesExplicitProject(t *testing.T) {
	o := &optOutServer{
		topics: map[string][]string{"g/quiet": {policy.TopicNoSummarize}},
		files:  map[string]bool{"g/filed": true},
		active: map[string]bool{"g/quiet": true, "g/filed": true},
	}
	gc := newOptOutClient(t, o)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		Projects: []string{"g/quiet", "g/filed", "g/wanted"},
		Since:    weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(projects) != 1 || projects[0] != "g/wanted" {
		t.Errorf("only the project with no opt-out signal should run, got %v", projects)
	}
}

// TestOptOutReason_NamesItsSource: a topic is invisible to anyone
// reading the repository, so the discovery log has to say which signal
// excluded a project or the skip is unexplainable.
func TestOptOutReason_NamesItsSource(t *testing.T) {
	o := &optOutServer{
		topics: map[string][]string{"g/t": {policy.TopicNoSummarize}},
		files:  map[string]bool{"g/f": true},
	}
	gc := newOptOutClient(t, o)
	ctx := context.Background()

	reason, err := optOutReason(ctx, gc, "g/t", &gitlab.ProjectSummary{Path: "g/t", Topics: []string{policy.TopicNoSummarize}, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, policy.TopicNoSummarize) {
		t.Errorf("reason should name the topic, got %q", reason)
	}

	reason, err = optOutReason(ctx, gc, "g/f", &gitlab.ProjectSummary{Path: "g/f", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Error("a project with the opt-out file must be excluded")
	}

	reason, err = optOutReason(ctx, gc, "g/clean", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("a project with no signal must run, got reason %q", reason)
	}
}

// TestNoSummarizeTopic_CaseSensitive: topics are exact tokens. GitLab
// topics are case-sensitive and we match them exactly, so a
// `No-Summarize` typo does not silently exclude anything.
func TestNoSummarizeTopic_CaseSensitive(t *testing.T) {
	o := &optOutServer{
		topics: map[string][]string{"g/typo": {"No-Summarize"}},
		files:  map[string]bool{},
		active: map[string]bool{"g/typo": true},
	}
	gc := newOptOutClient(t, o)

	projects, _, _, err := resolveProjects(context.Background(), gc, Options{
		DiscoverActive: true,
		Since:          weekStart(),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("a case-mismatched topic must not exclude; got %v", projects)
	}
}
