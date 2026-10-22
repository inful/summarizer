package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/safety"
)

// TestRunOneProject_SkipsCustomerForTeamOnlyProject: the dollar-value
// test for D5. When the team's classification says TeamOnly, the
// audience loop in runOneProject must skip the customer iteration.
// No LLM call, no customer-facing file. Team side still produces.
//
// We arrange team-only via InternalOnlyProjects (union source #2 of 3).
// The third source (the GitLab-side `summarize-team-only` topic) is
// tested in policy's own truth table.
//
// Both projects carry the `summarize` topic in the fixture, so
// customer output is the default and this test's team-only result is
// attributable to the override rather than to an empty topic list.
// TestRunOneProject_InternalOnlyIgnoresCase below is the case-variant
// companion; it uses its own server so the vacuous-fixture trap cannot
// bite it too.
func TestRunOneProject_SkipsCustomerForTeamOnlyProject(t *testing.T) {
	p, llm, pub := newTeamOnlyPipeline(t)
	p.Opts.InternalOnlyProjects = []string{"group/secret"}

	pr, err := p.runOneProject(context.Background(), "group/secret")
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}

	if !pr.TeamOnly {
		t.Errorf("ProjectResult.TeamOnly should be true; got false")
	}
	if pr.CustomerOn {
		t.Errorf("ProjectResult.CustomerOn should be false for team-only; got true")
	}
	if got := llm.customerCalls.Load(); got != 0 {
		t.Errorf("customer LLM calls: want 0, got %d", got)
	}
	if got := llm.teamCalls.Load(); got != 1 {
		t.Errorf("team LLM calls: want 1, got %d", got)
	}
	for _, f := range pr.FilesWritten {
		if strings.Contains(f, "customer") {
			t.Errorf("customer file written for team-only project: %s", f)
		}
	}
	// Written file(s) include the team file specifically.
	writtenNames := make([]string, 0, len(pub.written))
	for _, w := range pub.written {
		writtenNames = append(writtenNames, w.Name)
	}
	if !anyContains(writtenNames, "team") {
		t.Errorf("team file not written; got %v", writtenNames)
	}
}

// TestRunRollup_AllTeamOnlyProjects_NoCustomerFile: when every
// project in the run is team-only, runRollup must not produce a
// customer-facing rollup file. The team rollup is fine.
func TestRunRollup_AllTeamOnlyProjects_NoCustomerFile(t *testing.T) {
	p, _, _ := newTeamOnlyPipeline(t)
	pr := ProjectResult{
		Project:      "group/secret",
		CustomerOn:   false,
		TeamOnly:     true,
		CustomerText: "should-not-appear",
		TeamText:     "team content for secret project",
	}
	files, _, _, err := p.runRollup(context.Background(), []ProjectResult{pr})
	if err != nil {
		t.Fatalf("runRollup: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f, "customer") {
			t.Errorf("customer rollup file written when all projects team-only: %s", f)
		}
	}
	// One file: the team rollup.
	if got := len(files); got != 1 {
		t.Errorf("expected 1 file (team rollup), got %d: %v", got, files)
	}
}

// TestRunRollup_MixedPolicy_FiltersForCustomer: when the run has a
// mix of customer-on and team-only projects, the customer-facing
// rollup excludes the team-only project's body. The team-facing
// rollup includes all projects.
func TestRunRollup_MixedPolicy_FiltersForCustomer(t *testing.T) {
	p, _, _ := newTeamOnlyPipeline(t)
	pr1 := ProjectResult{
		Project: "group/public", CustomerOn: true, TeamOnly: false,
		CustomerText: "PUBLIC-MARKER", TeamText: "TEAM-FOR-PUBLIC",
	}
	pr2 := ProjectResult{
		Project: "group/secret", CustomerOn: false, TeamOnly: true,
		CustomerText: "SHOULD-NOT-APPEAR", TeamText: "TEAM-FOR-SECRET",
	}
	files, _, _, err := p.runRollup(context.Background(), []ProjectResult{pr1, pr2})
	if err != nil {
		t.Fatalf("runRollup: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 rollup files (customer+team), got %d", len(files))
	}
	// Inspect the rendered content to confirm the secret
	// project's body didn't bleed into the customer rollup's
	// stored body. The fake LLM returns a known marker; check
	// for it via the captured publisher body.
	customerBody := ""
	teamBody := ""
	for _, w := range p.Publisher.(*capturePublisher).written {
		if strings.Contains(w.Name, "customer") {
			customerBody += string(w.Body)
		}
		if strings.Contains(w.Name, "team") {
			teamBody += string(w.Body)
		}
	}
	if strings.Contains(customerBody, "SHOULD-NOT-APPEAR") {
		t.Errorf("customer rollup contains team-only content: %q", customerBody)
	}
	if !strings.Contains(teamBody, "TEAM-FOR-SECRET") {
		t.Errorf("team rollup missing team-only team's content (should include ALL projects): %q", teamBody)
	}
	if !strings.Contains(teamBody, "TEAM-FOR-PUBLIC") {
		t.Errorf("team rollup missing customer-on project's team content: %q", teamBody)
	}
	if !strings.Contains(customerBody, "PUBLIC-MARKER") {
		t.Errorf("customer rollup missing public content (should include customer-on project): %q", customerBody)
	}
}

// TestRunEpicRollup_MixedPolicy_FiltersForCustomer: same audience
// policy as the cross-project rollup, applied to the per-project
// epic entries.
func TestRunEpicRollup_MixedPolicy_FiltersForCustomer(t *testing.T) {
	p, _, _ := newTeamOnlyPipeline(t)
	pr1 := active(ProjectResult{Project: "group/public", CustomerOn: true}, pr1Epic())
	pr2 := active(ProjectResult{Project: "group/secret", TeamOnly: true}, pr2Epic())
	files, _, _, err := p.runEpicRollup(context.Background(), []ProjectResult{pr1, pr2})
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	// One customer file (pr1 is CustomerOn, its epic attributed to
	// ACME) plus the combined team file. There is no customer-side
	// combined file: that bucket only ever carried unattributed epics,
	// which are not customer-visible.
	if len(files) != 2 {
		t.Fatalf("expected 2 epic rollup files (acme customer + team combined), got %d: %v", len(files), files)
	}
	customerBody := ""
	for _, w := range p.Publisher.(*capturePublisher).written {
		if strings.Contains(w.Name, "epics") && strings.Contains(w.Name, "customer") {
			customerBody += string(w.Body)
		}
	}
	if strings.Contains(customerBody, "secret-epic") {
		t.Errorf("customer epic rollup contains team-only epic title: %q", customerBody)
	}
	if !strings.Contains(customerBody, "public-epic") {
		t.Errorf("customer epic rollup missing public epic title: %q", customerBody)
	}
}

// pr1Epic / pr2Epic return minimal *gitlab.Epic values. The
// pipeline's prompt code reads IID/Title/State/WebURL — the rest of
// the fields are zero-valued; we don't have to construct them.
// pr1Epic carries an org:: label because the customer audience is
// bucketed per customer: an epic nobody attributed produces no customer
// file at all, which is a different test.
func pr1Epic() *gitlab.Epic {
	return &gitlab.Epic{IID: 1, Title: "public-epic", State: "opened", WebURL: "u/1",
		Labels: []string{"org::ACME"}}
}
func pr2Epic() *gitlab.Epic {
	return &gitlab.Epic{IID: 2, Title: "secret-epic", State: "opened", WebURL: "u/2"}
}

// newTeamOnlyPipeline builds a Pipeline wired against a fake GitLab
// (returns empty MRs/issues/commits/epics — no activity), a counting
// LLM, and a capture publisher. The Pipeline's InternalOnlyProjects
// is left empty; tests set it as needed.
func newTeamOnlyPipeline(t *testing.T) (*Pipeline, *countingLLM, *capturePublisher) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(path, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/issues"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/repository/commits"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/epics"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/projects/group%2Fpublic") || strings.HasSuffix(path, "/projects/group%2Fsecret"):
			// `summarize` on BOTH projects, so customer output is the
			// default and the team-only result is attributable to the
			// audience decision rather than to an empty topic list.
			// With topics: [] Classify returns team-only whatever the
			// override says, which made every assertion below pass
			// without the feature doing anything.
			_, _ = w.Write([]byte(`{"namespace": {"kind": "group", "full_path": "group"}, "topics": ["summarize"]}`))
		default:
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	llmStub := &countingLLM{OnCall: func() string { return "rendered" }}
	pub := &capturePublisher{}
	pipe := &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(srv.URL, "glpat-test", 5*time.Second),
		LLM:       llmStub,
		Publisher: pub,
		Guard:     safety.NoopGuard{},
		Opts: Options{
			Projects: []string{"group/public", "group/secret"},
			Since:    weekStart(),
		},
	}
	return pipe, llmStub, pub
}

// countingLLM records audience-specific call counts. The pipeline
// audience loop runs Customer then Team; we sniff the system
// prompt to tell which audience each Complete call served.
type countingLLM struct {
	OnCall func() string

	calls         atomic.Int64
	customerCalls atomic.Int64
	teamCalls     atomic.Int64
	mu            sync.Mutex
}

func (c *countingLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls.Add(1)
	// Audience classification by system-prompt marker: "internal
	// engineering" → team, otherwise → customer. Tests use this to
	// verify the pipeline's per-audience iteration.
	if strings.Contains(req.System, "internal engineering") {
		c.teamCalls.Add(1)
	} else {
		c.customerCalls.Add(1)
	}
	// Echo back any markers the test seeded into the input prompt so
	// captured-body checks can verify what reached the LLM (and
	// therefore what would be published). The test fixtures seed
	// markers into per-project CustomerText / TeamText fields; we
	// extract those and echo them back. Default test body comes
	// from OnCall for tests that don't seed markers.
	body := c.OnCall()
	for _, marker := range []string{
		"PUBLIC-MARKER", "SHOULD-NOT-APPEAR",
		"public-epic", "secret-epic",
		"TEAM-FOR-PUBLIC", "TEAM-FOR-SECRET",
	} {
		if strings.Contains(req.User, marker) {
			body += "\n" + marker
		}
	}
	return llm.Response{Text: body, Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

// capturePublisher records every Publish call. Used to inspect the
// rollup files generated for the customer/team audience split.
type capturePublisher struct {
	mu      sync.Mutex
	written []publisher.Request
}

func (c *capturePublisher) Publish(_ context.Context, req publisher.Request) (publisher.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, req)
	return publisher.Result{Ref: "captured/" + req.Name}, nil
}

// anyContains returns true if any string in s contains substr.
func anyContains(s []string, substr string) bool {
	for _, x := range s {
		if strings.Contains(x, substr) {
			return true
		}
	}
	return false
}

// summarizeTopicsServer is a project carrying the `summarize` topic,
// which is the ONLY thing that turns on customer output. Everything
// else — the opt-out file is absent, no team-only topic — is arranged
// so that the internal-only list is the single reason a project comes
// out team-only. The shared newTeamOnlyPipeline fixture serves
// topics: [], which makes Classify return team-only no matter what the
// override says, so an override test built on it passes vacuously.
func summarizeTopicsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.Contains(path, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound) // no .summaryignore
		case strings.HasSuffix(path, "/merge_requests"),
			strings.HasSuffix(path, "/issues"),
			strings.HasSuffix(path, "/repository/commits"),
			strings.HasSuffix(path, "/epics"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespace": {"kind": "group", "full_path": "group"}, "topics": ["summarize"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunOneProject_InternalOnlyIgnoresCase: --project and
// --internal-only-project are both operator input, so a mismatch
// between them needs no GitLab misbehaviour at all — one command line
// naming the same project two ways marked it team-only and
// customer-facing simultaneously, and the customer document is
// published public: true.
//
// The pre-existing test above passes the same string on both sides and
// runs against a fixture whose empty topic list makes every project
// team-only anyway, so it cannot fail for the reason the feature
// exists. This one starts from a project that really would be
// customer-facing and checks the override is what stops it.
func TestRunOneProject_InternalOnlyIgnoresCase(t *testing.T) {
	// Sanity: without the override this project IS customer-facing, so
	// the assertions below cannot pass vacuously.
	base := &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		LLM:       &countingLLM{OnCall: func() string { return "rendered" }},
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
	}
	base.GitLab = gitlab.New(summarizeTopicsServer(t).URL, "glpat-test", 5*time.Second)
	pr, err := base.runOneProject(context.Background(), "group/secret")
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if !pr.CustomerOn {
		t.Fatalf("fixture is not exercising anything: a summarize project with no "+
			"override should be customer-facing, got TeamOnly=%v", pr.TeamOnly)
	}

	for _, typed := range []string{"Group/Secret", "GROUP/SECRET", "group/SECRET"} {
		p := &Pipeline{
			Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
			GitLab:    gitlab.New(summarizeTopicsServer(t).URL, "glpat-test", 5*time.Second),
			LLM:       &countingLLM{OnCall: func() string { return "rendered" }},
			Publisher: &capturePublisher{},
			Guard:     safety.NoopGuard{},
			Opts:      Options{InternalOnlyProjects: []string{"group/secret"}},
		}
		pr, err := p.runOneProject(context.Background(), typed)
		if err != nil {
			t.Fatalf("runOneProject(%q): %v", typed, err)
		}
		if pr.CustomerOn {
			t.Errorf("--project %q with --internal-only-project group/secret produced "+
				"customer-facing output; the override is a confidentiality control and "+
				"it fails open", typed)
		}
	}
}
