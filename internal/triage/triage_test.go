package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
)

// fakeDecide answers /v1/systemone with the given scores and counts the
// calls it received.
func fakeDecide(t *testing.T, blocked, peer float64) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake",
			"answers": map[string]any{
				"blocked":       map[string]any{"type": "noul", "noul": blocked},
				"peer_relevant": map[string]any{"type": "noul", "noul": peer},
			},
			"usage": map[string]any{
				"input_tokens": 100, "output_tokens": 20,
				"state_tokens": 50, "state_tokens_dropped": 0,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &bodies
}

func week(n int) []events.NormalizedEvent {
	var evs []events.NormalizedEvent
	for i := range n {
		evs = append(evs, events.NormalizedEvent{
			Kind: events.KindMR, Action: "merged", Ref: "#" + string(rune('a'+i)),
			Title: "merge request " + string(rune('a'+i)), Author: "dev",
		})
	}
	return evs
}

// TestTriage_WeekModeAsksOnceAboutTheWholeProject: one call per
// project, not one per event. The decision API's questions are
// state-level — a noul answers one probability for the whole state — so
// the week shape is the cheap one, and the only one whose cost does not
// scale with the size of the week.
func TestTriage_WeekModeAsksOnceAboutTheWholeProject(t *testing.T) {
	srv, calls, bodies := fakeDecide(t, 0.82, 0.4)
	tr := New(srv.URL, "tok", ModeWeek)

	res, err := tr.Analyse(context.Background(), "g/p", week(40))
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 for the whole project", calls.Load())
	}
	if res.Calls != 1 {
		t.Errorf("Result.Calls = %d, want 1", res.Calls)
	}
	if res.Blocked != 0.82 || res.PeerRelevant != 0.4 {
		t.Errorf("scores not carried through: %+v", res)
	}
	if res.InputTokens != 100 || res.OutputTokens != 20 {
		t.Errorf("token usage not carried through: %+v", res)
	}
	// The state must actually contain the events, or the classifier is
	// being asked about nothing.
	body := (*bodies)[0]
	for i := range 5 {
		if !strings.Contains(body, "merge request "+string(rune('a'+i))) {
			t.Errorf("the state does not mention event %d", i)
		}
	}
	// The questions are about THIS project's week, so the state has to
	// say which project is being judged.
	if !strings.Contains(body, "g/p") {
		t.Errorf("the state does not name the project:\n%s", body)
	}
}

// TestTriage_MRModeAsksPerMergeRequest: the other shape. One call per
// merge request, which is what per-item answers need — and what makes
// the cost scale with the size of the week.
func TestTriage_MRModeAsksPerMergeRequest(t *testing.T) {
	srv, calls, _ := fakeDecide(t, 0.1, 0.2)
	tr := New(srv.URL, "tok", ModeMR)

	res, err := tr.Analyse(context.Background(), "g/p", week(7))
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if calls.Load() != 7 {
		t.Errorf("calls = %d, want one per merge request (7)", calls.Load())
	}
	if res.Calls != 7 {
		t.Errorf("Result.Calls = %d, want 7", res.Calls)
	}
	if res.InputTokens != 700 {
		t.Errorf("input tokens = %d, want 700 (7 calls x 100)", res.InputTokens)
	}
}

// TestTriage_UnknownModeIsRejected: a mode that is not week or mr is a
// config typo, and silently treating it as one of them would make the
// run's cost depend on a spelling mistake.
func TestTriage_UnknownModeIsRejected(t *testing.T) {
	srv, _, _ := fakeDecide(t, 0, 0)
	tr := New(srv.URL, "tok", "per-event")
	if _, err := tr.Analyse(context.Background(), "g/p", week(1)); err == nil {
		t.Fatal("an unknown mode must be an error, not a guess")
	}
}

// TestTriage_EmptyWeekMakesNoCall: a project with no activity in the
// window has nothing to classify, and a call for it is pure cost.
func TestTriage_EmptyWeekMakesNoCall(t *testing.T) {
	srv, calls, _ := fakeDecide(t, 0.9, 0.9)
	tr := New(srv.URL, "tok", ModeWeek)

	res, err := tr.Analyse(context.Background(), "g/p", nil)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("calls = %d, want 0 for an empty week", calls.Load())
	}
	if res.Blocked != 0 {
		t.Errorf("an empty week must not report a blocker: %+v", res)
	}
}

// TestTriage_TransportErrorIsReturned: the caller decides what to do
// with a decision API that is down. Silently returning a zero score
// would make "no blockers found" indistinguishable from "nobody asked".
func TestTriage_TransportErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tr := New(srv.URL, "tok", ModeWeek)

	if _, err := tr.Analyse(context.Background(), "g/p", week(3)); err == nil {
		t.Fatal("a failing decision API must surface as an error")
	}
}

// TestTriage_ContextCancellationStops: a cancelled run must not keep
// calling the decision API.
func TestTriage_ContextCancellationStops(t *testing.T) {
	srv, _, _ := fakeDecide(t, 0.5, 0.5)
	tr := New(srv.URL, "tok", ModeMR)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.Analyse(ctx, "g/p", week(5)); err == nil {
		t.Fatal("a cancelled context must surface as an error")
	}
}

func TestTriage_ClientHasTimeout(t *testing.T) {
	tr := New("http://127.0.0.1:1", "tok", ModeWeek)
	if tr.Client == nil {
		t.Fatal("no client")
	}
	// A decision API that hangs must not hang the run.
	done := make(chan error, 1)
	go func() {
		_, err := tr.Analyse(context.Background(), "g/p", week(1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error against an unreachable endpoint")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the client has no usable timeout")
	}
}
