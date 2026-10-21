package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/policy"
	"github.com/inful/summarizer/internal/safety"
)

// Regression tests for the D5 safe-default being inverted.
//
// runOneProject used to classify with a hardcoded policy.SourceExplicit,
// because Run() documented an override to SourceDiscovered that it never
// performed. Every discovered project therefore classified as
// operator-named, so a project with NO topics produced a customer-facing
// report with Public: true — while the discovery manifest printed
// "(team-only)" from a correct, separately-computed classification. The
// origin a project actually came from now flows into the classifier.

// sourcePipeline builds a Pipeline whose fake GitLab answers every
// endpoint the per-project path touches, with the given project topics.
// Returns the pipeline plus the fakes so tests can assert on LLM calls
// and published files.
type sourcePipeline struct {
	p     *Pipeline
	llm   *recordingLLM
	pub   *capturePublisher
	topic []string
}

func newSourcePipeline(t *testing.T, topics []string, projects []string) *sourcePipeline {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(path, "/groups/group/projects"):
			// Discovery: one project in the group, no topics.
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id": 1, "path_with_namespace": "group/public",
				"last_activity_at": "2026-10-19T00:00:00Z"}]`))
		case strings.HasSuffix(path, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("per_page") == "1" {
				// Discovery activity probe: non-empty means active.
				_, _ = w.Write([]byte(`[{"iid": 1}]`))
				return
			}
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
		case strings.HasPrefix(path, "/api/v4/projects/group%2F"):
			// Project-summary / namespace endpoint. Topics drive the
			// classifier; namespace says "group" so the epics path is
			// exercised (and returns empty above).
			b, _ := json.Marshal(map[string]any{
				"namespace": map[string]string{"kind": "group", "full_path": "group"},
				"topics":    topics,
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		default:
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	llmStub := &recordingLLM{}
	pub := &capturePublisher{}
	pipe := &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(srv.URL, "glpat-test", 5*time.Second),
		LLM:       llmStub,
		Publisher: pub,
		Guard:     safety.NoopGuard{},
		Opts: Options{
			Projects: projects,
			Since:    168 * time.Hour,
		},
	}
	return &sourcePipeline{p: pipe, llm: llmStub, pub: pub, topic: topics}
}

// recordingLLM is a test double that captures every Complete call so
// tests can assert on the prompts and response shapes, plus counts
// audience-specific calls (audience is identified by the same marker
// strings the per-project system prompt uses, so the team-audience
// prompts — which contain "internal engineering" — and the
// customer-audience prompts can be distinguished).
type recordingLLM struct {
	mu            sync.Mutex
	calls         []recordedCall
	customerCalls int
	teamCalls     int
}

type recordedCall struct {
	userPrompt string
}

func (r *recordingLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{userPrompt: req.User})
	if strings.Contains(req.System, "internal engineering") {
		r.teamCalls++
	} else {
		r.customerCalls++
	}
	r.mu.Unlock()
	return llm.Response{
		Text:  "rendered",
		Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// promptAt returns the user-prompt sent to the i-th LLM call (0-based).
func (r *recordingLLM) promptAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.calls) {
		return ""
	}
	return r.calls[i].userPrompt
}

// TestRunOneProject_DiscoveredWithoutOptInIsTeamOnly: the headline case.
func TestRunOneProject_DiscoveredWithoutOptInIsTeamOnly(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)

	pr, err := sp.p.runOneProject(context.Background(), "group/public", policy.SourceDiscovered)
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if !pr.TeamOnly || pr.CustomerOn {
		t.Errorf("discovered + no topics must be team-only; TeamOnly=%v CustomerOn=%v", pr.TeamOnly, pr.CustomerOn)
	}
	if got := sp.llm.customerCalls; got != 0 {
		t.Errorf("customer LLM calls: want 0, got %d", got)
	}
	if got := sp.llm.teamCalls; got != 1 {
		t.Errorf("team LLM calls: want 1, got %d", got)
	}
	for _, w := range sp.pub.written {
		if strings.Contains(w.Name, "customer") {
			t.Errorf("customer file written for a team-only project: %s", w.Name)
		}
		if w.Public {
			t.Errorf("no file from a team-only project may be Public: %s", w.Name)
		}
	}
}

// TestRunOneProject_DiscoveredWithOptInIsCustomerOn: the `summarize`
// topic flips the discovered default back on.
func TestRunOneProject_DiscoveredWithOptInIsCustomerOn(t *testing.T) {
	sp := newSourcePipeline(t, []string{"summarize"}, nil)

	pr, err := sp.p.runOneProject(context.Background(), "group/public", policy.SourceDiscovered)
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if pr.TeamOnly || !pr.CustomerOn {
		t.Errorf("discovered+summarize must be customer-on; TeamOnly=%v CustomerOn=%v", pr.TeamOnly, pr.CustomerOn)
	}
	if got := sp.llm.customerCalls; got != 1 {
		t.Errorf("customer LLM calls: want 1, got %d", got)
	}
	sawPublic := false
	for _, w := range sp.pub.written {
		if strings.Contains(w.Name, "customer") {
			sawPublic = true
			if !w.Public {
				t.Errorf("customer report must carry Public=true: %s", w.Name)
			}
		}
	}
	if !sawPublic {
		t.Error("no customer report published")
	}
}

// TestRunOneProject_ExplicitNoTopicsIsTeamOnly: naming a project by
// hand is no longer enough to unlock customer-facing output. The
// default is team-only for every project regardless of how it entered
// the run; only the `summarize` topic promotes it.
func TestRunOneProject_ExplicitNoTopicsIsTeamOnly(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)

	pr, err := sp.p.runOneProject(context.Background(), "group/public", policy.SourceExplicit)
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if !pr.TeamOnly || pr.CustomerOn {
		t.Errorf("explicit project with no topics must be team-only; TeamOnly=%v CustomerOn=%v", pr.TeamOnly, pr.CustomerOn)
	}
}

// TestRunOneProject_TopicTeamOnlyOverridesOptIn: `summarize-team-only`
// wins over `summarize`, for discovered and explicit alike.
func TestRunOneProject_TopicTeamOnlyOverridesOptIn(t *testing.T) {
	for _, src := range []policy.ProjectSource{policy.SourceDiscovered, policy.SourceExplicit} {
		sp := newSourcePipeline(t, []string{"summarize", "summarize-team-only"}, nil)
		pr, err := sp.p.runOneProject(context.Background(), "group/public", src)
		if err != nil {
			t.Fatalf("runOneProject: %v", err)
		}
		if !pr.TeamOnly {
			t.Errorf("source=%v: summarize-team-only must win over summarize", src)
		}
	}
}

// TestPipelineRun_DiscoveredProjectReachesRunOneProjectAsDiscovered:
// end-to-end through Run(), so the plumbing that carries discoveredSet
// into runOneProject is covered — not just runOneProject's parameter.
func TestPipelineRun_DiscoveredProjectReachesRunOneProjectAsDiscovered(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	sp.p.Opts.DiscoveredGroups = []string{"group"}

	res, err := sp.p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Projects) != 1 {
		t.Fatalf("expected 1 project in the run, got %d", len(res.Projects))
	}
	got := res.Projects[0]
	if !got.FromDiscovery {
		t.Error("FromDiscovery should be true for a discovered project")
	}
	if got.CustomerOn {
		t.Error("a discovered project with no topics must not reach the customer audience")
	}
	for _, w := range sp.pub.written {
		if w.Public {
			t.Errorf("Run() published a Public file from a discovered team-only project: %s", w.Name)
		}
	}
}

// TestPipelineRun_ExplicitProjectIsNotMarkedDiscovered: the mirror case,
// so the flag can't drift the other way and suppress an operator's
// explicit listing.
func TestPipelineRun_ExplicitProjectIsNotMarkedDiscovered(t *testing.T) {
	sp := newSourcePipeline(t, nil, []string{"group/public"})

	res, err := sp.p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(res.Projects))
	}
	if res.Projects[0].FromDiscovery {
		t.Error("an explicit --project must not be marked as discovered")
	}
}
