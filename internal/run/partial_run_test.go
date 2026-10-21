package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/safety"
)

// failingLLM fails for prompts naming a chosen project and succeeds
// otherwise, so a run can be steered into a mid-loop failure.
type failingLLM struct {
	failOn string
	calls  int
}

func (f *failingLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.calls++
	if f.failOn != "" && strings.Contains(req.User, f.failOn) {
		return llm.Response{}, context.DeadlineExceeded
	}
	return llm.Response{Text: "rendered"}, nil
}

// twoProjectPipeline wires a Pipeline over a fake GitLab serving two
// explicit projects, with the LLM failing on the second.
func twoProjectPipeline(t *testing.T, failOn string) (*Pipeline, *failingLLM) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(p, "/merge_requests"),
			strings.Contains(p, "/issues"),
			strings.Contains(p, "/repository/commits"),
			strings.Contains(p, "/epics"):
			// List endpoints want an array; the project-summary
			// endpoint (the default branch below) wants an object.
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main","topics":[]}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	stub := &failingLLM{failOn: failOn}
	pub := &capturePublisher{}
	return &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(srv.URL, "glpat-x", 5*time.Second),
		LLM:       stub,
		Publisher: pub,
		Guard:     safety.NoopGuard{},
		Opts: Options{
			Projects: []string{"group/aaa", "group/zzz"},
			Since:    168 * time.Hour,
		},
	}, stub
}

// TestRun_ContinuesPastAFailedProject: a failure on one project must not
// abort the rest. The old loop returned on the first error, so a
// rate-limited call on project 1 of 25 meant 24 projects never ran while
// the exit code said the run had failed.
func TestRun_ContinuesPastAFailedProject(t *testing.T) {
	p, stub := twoProjectPipeline(t, "zzz")

	res, err := p.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error so the cron wrapper still sees a failure")
	}
	if !strings.Contains(err.Error(), "zzz") {
		t.Errorf("error should name the failing project, got: %v", err)
	}
	// The failing project must NOT appear as a result (it would be a
	// zero-value ProjectResult with an empty name in the summary).
	if len(res.Projects) != 1 {
		var names []string
		for _, pr := range res.Projects {
			names = append(names, pr.Project)
		}
		t.Fatalf("want only the surviving project, got %v", names)
	}
	if res.Projects[0].Project != "group/aaa" {
		t.Errorf("survivor should be group/aaa, got %q", res.Projects[0].Project)
	}
	// The surviving project still produced its files.
	if len(res.Projects[0].FilesWritten) == 0 {
		t.Error("the project that succeeded should have written files")
	}
	if stub.calls == 0 {
		t.Error("LLM should have been called for the surviving project")
	}
}

// TestRun_SkipsRollupsOnPartialFailure: a cross-project rollup over a
// partial project set would read as a complete week. The run returns
// before the rollup step.
func TestRun_SkipsRollupsOnPartialFailure(t *testing.T) {
	p, _ := twoProjectPipeline(t, "zzz")
	res, err := p.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(res.RollupFiles) != 0 {
		t.Errorf("rollup must not run over a partial project set, got %v", res.RollupFiles)
	}
}

// TestRun_ExplicitProjectsOutrankTheCap: --max-projects is a cost guard
// for a wide discovery sweep, not a filter on what the operator typed.
// The old code sorted explicit and discovered entries into one pool and
// cut it, so `--project group/zzz` was silently dropped whenever a
// discovered project sorted ahead of it.
func TestRun_ExplicitProjectsOutrankTheCap(t *testing.T) {
	p, stub := twoProjectPipeline(t, "")
	p.Opts.MaxProjects = 1

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Projects) != 2 {
		t.Fatalf("explicit listings must all run even over the cap, got %d", len(res.Projects))
	}
	for _, pr := range res.Projects {
		if pr.Project != "group/aaa" && pr.Project != "group/zzz" {
			t.Errorf("unexpected project %q", pr.Project)
		}
	}
	// Two team-only projects plus the cross-project team rollup.
	if stub.calls != 3 {
		t.Errorf("expected 3 completions (2 projects + 1 rollup), got %d", stub.calls)
	}
}

// TestRun_MaxProjectsDefaultsWhenUnset: 0 means "use the default", not
// "no limit" — an unset flag must still bound the bill.
func TestRun_MaxProjectsDefaultsWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want int
	}{
		{"unset uses the default", Options{}, DefaultMaxProjects},
		{"negative uses the default", Options{MaxProjects: -1}, DefaultMaxProjects},
		{"explicit below the default is honoured", Options{MaxProjects: 3}, 3},
		{"explicit above the default is honoured", Options{MaxProjects: 500}, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.opts.maxProjects(); got != c.want {
				t.Errorf("maxProjects() = %d, want %d", got, c.want)
			}
		})
	}
	if DefaultMaxProjects <= 0 {
		t.Error("the default cap must be a positive number")
	}
}

// TestEpicEventCounts_ExactNotTitleMatch: the count must come from real
// epic annotations. The previous implementation counted the epic's title
// inside the rendered report body, so two epics sharing a title each
// reported the sum of both — and the number reached customer-facing
// documents.
func TestEpicEventCounts_ExactNotTitleMatch(t *testing.T) {
	epicA := &gitlab.Epic{IID: 1, Title: "Billing rewrite"}
	epicB := &gitlab.Epic{IID: 2, Title: "Billing rewrite"} // same title, different epic

	evs := []events.NormalizedEvent{
		{Kind: events.KindIssue, Epic: epicA},
		{Kind: events.KindIssue, Epic: epicA},
		{Kind: events.KindIssue, Epic: epicB},
		{Kind: events.KindMR}, // no epic annotation
	}
	counts := epicEventCounts(evs, []*gitlab.Epic{epicA, epicB})

	if counts[1] != 2 {
		t.Errorf("epic 1: got %d, want 2", counts[1])
	}
	if counts[2] != 1 {
		t.Errorf("epic 2 with the same title: got %d, want 1", counts[2])
	}
	// An epic with no annotated events is present with a zero, not absent
	// — the rollup still lists it, with "0 events".
	counts = epicEventCounts(evs, []*gitlab.Epic{epicA, epicB, {IID: 99}})
	if got, ok := counts[99]; !ok || got != 0 {
		t.Errorf("unrelated epic: got (%d, %v), want (0, true)", got, ok)
	}
	// Nil epics in the slice must not panic.
	if got := epicEventCounts(evs, []*gitlab.Epic{nil}); len(got) != 0 {
		t.Errorf("nil epic: got %v, want empty", got)
	}
}
