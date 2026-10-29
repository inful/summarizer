// Package triage asks the decision API (inful/decide) what a project's
// week of activity looks like, before the prompt is rendered.
//
// Why a classifier and not the LLM: the team prompt asks for things the
// collected events cannot answer. "What is blocked" has no producer at
// all — nothing in the pipeline fetches anything that means blocked —
// so the model either invents blockers or says there are none. A
// classifier is the right shape for that question: cheap, fast, and
// asked once.
//
// What it CANNOT do is point at WHICH merge request is blocked. The
// decision API's noul question answers one probability for the whole
// state, so the answer is "there is blocked work this week", never "MR
// #7 is blocked". That is why the result is a HINT and not a finding:
// the prompt tells the model that a classifier noticed something, and
// the model — which sees every event with full context — says which one
// it was and what it is waiting on.
//
// Two call shapes, measured in the benchmark in internal/prompt:
//
//	week — one call per project, over the project's whole event list.
//	       Cheap, and the cost does not scale with the size of the week.
//	mr   — one call per merge request. Only this shape can give
//	       per-item answers, and the cost is one call per event.
//
// The default is week. See ModeWeek and ModeMR.
package triage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/inful/decide"
	"github.com/inful/summarizer/internal/events"
)

// The two call shapes.
const (
	// ModeWeek makes one call per project, over the project's whole
	// event list. One call, one answer per question about the WEEK.
	ModeWeek = "week"
	// ModeMR makes one call per merge request. One call per event, and
	// the only shape that can answer per item.
	ModeMR = "mr"
)

// Result is what the decision API said about a project's activity.
//
// The scores are probabilities in [0,1], not verdicts. A caller that
// treats 0.5 as "blocked" is inventing a threshold the API does not
// have; the prompt presents the score and lets the model judge the
// evidence in front of it.
type Result struct {
	// Blocked is the probability that this activity contains work
	// waiting on something outside itself.
	Blocked float64
	// PeerRelevant is the probability that a team depending on this
	// project needs to know about it.
	PeerRelevant float64

	// Calls, InputTokens and OutputTokens are what the classification
	// cost. They are reported in the run summary so an operator can see
	// what a mode costs before choosing it — the numbers differ by two
	// orders of magnitude between the two shapes.
	Calls        int
	InputTokens  int
	OutputTokens int
}

// Triage classifies a project's activity.
type Triage struct {
	Client *decide.Client
	Mode   string
}

// New builds a Triage against a /v1/systemone endpoint. mode must be
// ModeWeek or ModeMR; an unknown mode is rejected by Analyse rather
// than guessed at, because the two shapes differ in cost by two orders
// of magnitude and a typo should not silently pick one.
func New(baseURL, token, mode string) *Triage {
	opts := []decide.Option{
		// A decision API that accepts the connection and never answers
		// would otherwise hold a project open for the whole run. The
		// caller's context is the outer bound; this is the inner one.
		decide.WithHTTPClient(&http.Client{Timeout: Timeout}),
	}
	if token != "" {
		opts = append(opts, decide.WithToken(token))
	}
	return &Triage{
		Client: decide.New(baseURL, opts...),
		Mode:   mode,
	}
}

// The questions asked of the decision API. The same two are asked in
// both modes; what differs is the state they are asked about.
//
// The wording is long on purpose: these instructions are the model
// being asked a subtle question, and "the state" is the thing being
// judged. Measured against a 4K-context model, the instructions cost
// more tokens than the event they describe — but shortening them moved
// the cost from input to output rather than removing it, so the lever
// is the call count, not the wording.
//
// Both are nouls: a probability for the whole state. That is the shape
// the decision API has, and it is the reason the result is a hint and
// not a finding — see the package comment.
func questions() decide.Questions {
	return decide.Questions{
		"blocked": decide.NewNoul(
			"Does this activity describe work that is blocked, waiting on, or dependent " +
				"on something outside itself — another team, another system, a pending " +
				"decision, a pending review? Answer no for ordinary work that is simply " +
				"in progress."),
		"peer_relevant": decide.NewNoul(
			"Would a peer team that depends on this work need to know about it — a changed " +
				"interface, a new capability, a deprecation, a behaviour change they might " +
				"be affected by, or a shared dependency?"),
	}
}

// Analyse classifies the given events. It returns the scores the API
// gave, plus what the classification cost.
//
// An empty week makes no call: there is nothing to classify, and a
// call for it is pure cost.
func (t *Triage) Analyse(ctx context.Context, project string, evs []events.NormalizedEvent) (Result, error) {
	switch t.Mode {
	case ModeWeek, ModeMR:
	default:
		return Result{}, fmt.Errorf("triage: unknown mode %q (want %q or %q)", t.Mode, ModeWeek, ModeMR)
	}
	if len(evs) == 0 {
		return Result{}, nil
	}

	if t.Mode == ModeMR {
		return t.analysePerMR(ctx, project, evs)
	}
	return t.analyseWeek(ctx, project, evs)
}

// analyseWeek asks once, about the whole project.
func (t *Triage) analyseWeek(ctx context.Context, project string, evs []events.NormalizedEvent) (Result, error) {
	resp, err := t.Client.SystemOne(ctx, decide.Request{
		State:     decide.State{Body: state(project, eventList(evs))},
		Questions: questions(),
	})
	if err != nil {
		return Result{}, fmt.Errorf("triage: %w", err)
	}
	return resultFrom(resp), nil
}

// analysePerMR asks once per merge request. Other event kinds are not
// asked about: a commit or an issue does not describe a change the way
// a merge request does, and asking about them would cost a call each
// for a weaker answer.
func (t *Triage) analysePerMR(ctx context.Context, project string, evs []events.NormalizedEvent) (Result, error) {
	var out Result
	for _, e := range evs {
		if e.Kind != events.KindMR {
			continue
		}
		resp, err := t.Client.SystemOne(ctx, decide.Request{
			State:     decide.State{Body: state(project, eventText(e))},
			Questions: questions(),
		})
		if err != nil {
			return out, fmt.Errorf("triage: %w", err)
		}
		r := resultFrom(resp)
		out.Calls += r.Calls
		out.InputTokens += r.InputTokens
		out.OutputTokens += r.OutputTokens
		// The week's verdict is the strongest per-item signal seen, not
		// an average: one blocked merge request in a quiet week is the
		// thing worth reporting, and averaging it with fifty chores
		// would bury it.
		if r.Blocked > out.Blocked {
			out.Blocked = r.Blocked
		}
		if r.PeerRelevant > out.PeerRelevant {
			out.PeerRelevant = r.PeerRelevant
		}
	}
	return out, nil
}

// resultFrom reads the answers the API gave.
func resultFrom(resp *decide.Response) Result {
	out := Result{Calls: 1, InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	if a, ok := resp.Answers["blocked"]; ok && a.IsNoul() {
		out.Blocked = a.NoulValue()
	}
	if a, ok := resp.Answers["peer_relevant"]; ok && a.IsNoul() {
		out.PeerRelevant = a.NoulValue()
	}
	return out
}

// state names the project the activity belongs to, then the activity
// itself. The questions are about THIS project's week ("a peer team
// that depends on this work"), so the classifier is told which project
// it is judging rather than left to guess from the event lines.
func state(project, body string) string {
	return "Project: " + project + "\n\n" + body
}

// eventList renders a project's week as the state the classifier reads.
// The shape mirrors the prompt's event lines, so the classifier sees
// what the LLM will see.
func eventList(evs []events.NormalizedEvent) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString("- [")
		b.WriteString(string(e.Kind))
		b.WriteString(" ")
		b.WriteString(e.Ref)
		b.WriteString("] (")
		b.WriteString(e.Action)
		b.WriteString(", by ")
		b.WriteString(e.Author)
		b.WriteString(") ")
		b.WriteString(e.Title)
		if len(e.Labels) > 0 {
			b.WriteString(" (tags: ")
			b.WriteString(strings.Join(e.Labels, ","))
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// eventText renders one merge request for the per-MR shape, with the
// description when there is one: the description is where a blocker is
// written down.
func eventText(e events.NormalizedEvent) string {
	var b strings.Builder
	b.WriteString(e.Title)
	b.WriteString("\n")
	if e.TeamOnlyDescription != "" {
		b.WriteString(e.TeamOnlyDescription)
		b.WriteString("\n")
	}
	if len(e.Labels) > 0 {
		b.WriteString("labels: ")
		b.WriteString(strings.Join(e.Labels, ", "))
		b.WriteString("\n")
	}
	return b.String()
}

// Timeout is how long ONE classification call may take. A decision API
// that hangs must not hang the run: the caller's context is the outer
// bound, but a decision API that accepts the connection and never
// answers would otherwise hold a project open indefinitely.
//
// It bounds one call, not one project. In ModeMR a project makes one
// call per merge request, so a hanging API costs up to Timeout per
// merge request — the run-level deadline is what bounds the mode.
const Timeout = 30 * time.Second
