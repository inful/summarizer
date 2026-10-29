package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/safety"
	"github.com/inful/summarizer/internal/triage"
)

// fakeDecide answers /v1/systemone with fixed scores and counts the
// calls it received.
func fakeDecide(t *testing.T, blocked, peer float64, fail bool) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/systemone", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake",
			"answers": map[string]any{
				"blocked":       map[string]any{"type": "noul", "noul": blocked},
				"peer_relevant": map[string]any{"type": "noul", "noul": peer},
			},
			"usage": map[string]any{
				"input_tokens": 120, "output_tokens": 30,
				"state_tokens": 60, "state_tokens_dropped": 0,
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

// triageFixture builds a Pipeline with one merge request in the window,
// so there is a week to classify. The project carries the `summarize`
// topic, so customer output is on by default.
func triageFixture(t *testing.T, decideURL string) (*Pipeline, *countingLLM) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/groups/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case strings.Contains(p, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"iid": 7, "title": "Ship the exporter", "state": "merged",
				"description": "Blocked on the finance API.",
				"updated_at":  now.Format(time.RFC3339),
				"merged_at":   now.Format(time.RFC3339),
			}})
		case strings.Contains(p, "/repository/files/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(p, "/issues"), strings.Contains(p, "/repository/commits"), strings.Contains(p, "/epics"):
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"default_branch":"main","namespace":{"kind":"group","full_path":"group"},"topics":["summarize"]}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	llmStub := &countingLLM{OnCall: func() string { return "rendered" }}
	pipe := &Pipeline{
		Cfg:       &config.Config{OutputDir: t.TempDir(), MaxEvents: 200},
		GitLab:    gitlab.New(srv.URL, "glpat-test", 5*time.Second),
		LLM:       llmStub,
		Publisher: &capturePublisher{},
		Guard:     safety.NoopGuard{},
		Opts:      Options{Projects: []string{"group/public"}, Since: weekStart()},
	}
	if decideURL != "" {
		pipe.Triage = triage.New(decideURL, "tok", triage.ModeWeek)
	}
	return pipe, llmStub
}

// TestRunOneProject_TriageHintReachesTheTeamPromptOnly: the whole
// point of the classifier is that the team prompt asks about blockers
// only when something in the data suggests there might be some — and
// that the note saying so stays out of the customer document, which is
// published public: true.
func TestRunOneProject_TriageHintReachesTheTeamPromptOnly(t *testing.T) {
	url, calls := fakeDecide(t, 0.83, 0.6, false)
	p, llmStub := triageFixture(t, url)

	pr, err := p.runOneProject(context.Background(), "group/public")
	if err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("classifier calls = %d, want 1", calls.Load())
	}
	if pr.Triage.Calls != 1 || pr.Triage.InputTokens != 120 || pr.Triage.OutputTokens != 30 {
		t.Errorf("the classification cost was not recorded: %+v", pr.Triage)
	}
	if pr.TriageFailed {
		t.Error("TriageFailed is true on a successful classification")
	}

	team := llmStub.promptFor(t, true)
	if !strings.Contains(team, "what is blocked") {
		t.Errorf("the team prompt does not ask about blockers:\n%s", team)
	}
	if !strings.Contains(team, "0.83") {
		t.Errorf("the classifier's score is not in the team prompt:\n%s", team)
	}

	customer := llmStub.promptFor(t, false)
	if strings.Contains(customer, "0.83") || strings.Contains(customer, "Classifier note") {
		t.Errorf("the classifier note reached the customer prompt:\n%s", customer)
	}
}

// TestRunOneProject_TriageFailureIsNonFatal: a decision API that is
// down must not take the week's reports with it. The project still
// runs, still publishes, and says plainly that it published without the
// hint — a report that silently lost its blockers section reads exactly
// like a report that found none.
func TestRunOneProject_TriageFailureIsNonFatal(t *testing.T) {
	url, _ := fakeDecide(t, 0.9, 0.9, true)
	p, llmStub := triageFixture(t, url)

	pr, err := p.runOneProject(context.Background(), "group/public")
	if err != nil {
		t.Fatalf("a failing classifier must not fail the project: %v", err)
	}
	if !pr.TriageFailed {
		t.Error("TriageFailed is false after the classifier failed")
	}
	if len(pr.FilesWritten) == 0 {
		t.Error("no file written: the report must still be published")
	}
	team := llmStub.promptFor(t, true)
	if strings.Contains(team, "Classifier note") {
		t.Errorf("a failed classification still produced a hint:\n%s", team)
	}
}

// TestRunOneProject_NoTriageMeansNoClassifierNote: with no classifier
// configured there is nothing that knows whether anything is blocked,
// so the prompt must not ask about it. Asking a question the data
// cannot answer is what produced invented blockers before.
func TestRunOneProject_NoTriageMeansNoClassifierNote(t *testing.T) {
	p, llmStub := triageFixture(t, "")

	if _, err := p.runOneProject(context.Background(), "group/public"); err != nil {
		t.Fatalf("runOneProject: %v", err)
	}
	team := llmStub.promptFor(t, true)
	if strings.Contains(team, "Classifier note") {
		t.Errorf("a classifier note appeared with no classifier:\n%s", team)
	}
	if strings.Contains(team, "what is blocked") {
		t.Errorf("the prompt asks about blockers with nothing to find them in:\n%s", team)
	}
}
