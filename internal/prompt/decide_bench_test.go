package prompt

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inful/decide"
	"github.com/inful/summarizer/internal/events"
)

// DECIDE_URL points this at a /v1/systemone endpoint. DECIDE_TOKEN is
// optional. Without DECIDE_URL the benchmark skips.
//
// It measures the shape of the triage call the team prompt could use:
// one four-question decide call per merge request, against one call per
// project over the week's event list.
//
// The numbers that decide the design are the CALL COUNT and the INPUT
// TOKENS, not the absolute latency: those are properties of the request,
// not of the model behind it. A per-MR design costs one call per event;
// a per-project design costs one call per project. Re-run this when the
// decision model changes.
//
// Output shape matches the API's own: a noul question answers one
// probability for the whole state, so a per-project call can say "there
// is blocked work this week" but never "MR #7 is blocked". Per-item
// verdicts need per-item calls.

// realisticWeek builds a plausible 200-event week for one busy project:
// merged and open merge requests, issues, and commits.
func realisticWeek(n int) ([]events.NormalizedEvent, []events.NormalizedEvent) {
	var all, mrs []events.NormalizedEvent
	subjects := []string{
		"Fix invoice rounding for multi-currency totals",
		"Extract the retry layer out of the LLM provider interface",
		"Add CSV export to the billing dashboard",
		"Drop the deprecated v1 reports endpoint",
		"Cache project namespace lookups between runs",
		"Handle 429s from the GitLab API with jitter",
		"Migrate the scheduler to the new cron syntax",
		"Fix a nil dereference when a project has no default branch",
		"Add integration tests for the publisher retry path",
		"Reduce peak memory in the diff cap",
	}
	bodies := []string{
		"Reworked the rounding so multi-currency invoices sum without drift across line items. Blocked on the finance API schema landing first.",
		"The provider interface had grown three nested conditionals. Pulled the retry policy into its own type with a test for each backoff case.",
		"Operators asked for a CSV of the billing dashboard. Reads the existing query path, no new permissions.",
		"The v1 endpoint has been unused for two releases. Removing it and the fixture that covered it.",
		"Namespace lookups were repeated per project. A small memo keyed by group path removes most of the traffic.",
	}
	for i := range n {
		kind, action := events.KindMR, "merged"
		if i%5 == 0 {
			action = "opened"
		}
		if i%7 == 0 {
			kind, action = events.KindIssue, "opened"
		}
		if i%11 == 0 {
			kind, action = events.KindCommit, "pushed"
		}
		ev := events.NormalizedEvent{
			Kind:     kind,
			Action:   action,
			Ref:      fmt.Sprintf("#%d", i+1),
			Title:    fmt.Sprintf("%s (%d)", subjects[i%len(subjects)], i+1),
			Author:   fmt.Sprintf("dev%d", i%9),
			URL:      fmt.Sprintf("http://gitlab.example.com/g/p/-/merge_requests/%d", i+1),
			Labels:   []string{"backend", fmt.Sprintf("area-%d", i%4)},
			Comments: i % 7,
			Created:  time.Now().Add(-time.Duration(i) * time.Hour),
			Updated:  time.Now().Add(-time.Duration(i) * time.Minute),
		}
		if kind == events.KindMR {
			ev.TeamOnlyDescription = bodies[i%len(bodies)]
			mrs = append(mrs, ev)
		}
		all = append(all, ev)
	}
	return all, mrs
}

// triageQuestions is the four-question set the prototype asks per merge
// request: the four things the team prompt currently has to infer from a
// title, or has no data for at all.
func triageQuestions() decide.Questions {
	return decide.Questions{
		"kind": decide.NewChoice("What kind of change is this merge request?",
			map[string]string{
				"feature":  "a new capability users or operators can see",
				"bugfix":   "fixing a defect or incorrect behaviour",
				"refactor": "restructuring without changing behaviour",
				"chore":    "housekeeping, dependencies, config, build",
				"docs":     "documentation only",
				"test":     "tests only",
			}),
		"blocked": decide.NewNoul(
			"Does this change describe work that is blocked, waiting on, or dependent " +
				"on something outside this change — another team, another system, a " +
				"pending decision? Answer no if it is ordinary work."),
		"peer_relevant": decide.NewNoul(
			"Would a peer team that depends on this project need to know about this " +
				"change — a changed interface, a new capability, a deprecation, a " +
				"behaviour change they might be affected by?"),
		"worth_reporting": decide.NewNoul(
			"Is this change worth a line in a weekly engineering digest for the owning " +
				"team? Answer no for trivial or purely internal housekeeping."),
	}
}

// TestBenchmark_DecideTriageShapes: the call-shape benchmark behind the
// decision on whether decide should triage merge requests.
//
// Per-MR: one four-question call per merge request, so one call per
// event — measured over a realistic 200-event week.
//
// Per-project: one call over the whole week's event list — one call per
// project, but a noul answers one probability for the whole state, so
// it can say "there is blocked work this week" and never "MR #7 is
// blocked".
func TestBenchmark_DecideTriageShapes(t *testing.T) {
	base := os.Getenv("DECIDE_URL")
	if base == "" {
		t.Skip("set DECIDE_URL to a /v1/systemone endpoint to run this benchmark")
	}
	opts := []decide.Option{}
	if tok := os.Getenv("DECIDE_TOKEN"); tok != "" {
		opts = append(opts, decide.WithToken(tok))
	}
	client := decide.New(base, opts...)

	all, mrs := realisticWeek(200)

	// The LLM baseline: the real team prompt for this project.
	_, user, err := Render(AudienceTeam, "group/sub/project", all, 200, nil, Hint{}, Limits{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	fmt.Printf("BASELINE  team prompt for %d events: %d bytes (~%d tokens)\n",
		len(all), len(user), len(user)/4)

	questions := triageQuestions()
	stateOf := func(m events.NormalizedEvent) string {
		return m.Title + "\n" + m.TeamOnlyDescription + "\nlabels: " + strings.Join(m.Labels, ", ")
	}

	// PER-MR: one call per merge request.
	sample := mrs
	if len(sample) > 20 {
		sample = sample[:20]
	}
	var (
		mu            sync.Mutex
		inTok, outTok int
	)
	start := time.Now()
	for _, m := range sample {
		r, err := client.SystemOne(context.Background(), decide.Request{
			State: decide.State{Body: stateOf(m)}, Questions: questions,
		})
		if err != nil {
			t.Errorf("per-MR call: %v", err)
			break
		}
		mu.Lock()
		inTok += r.Usage.InputTokens
		outTok += r.Usage.OutputTokens
		mu.Unlock()
	}
	perMR := time.Since(start) / time.Duration(len(sample))
	fmt.Printf("PER-MR    %d calls: %d in / %d out tokens, %s per call\n",
		len(sample), inTok, outTok, perMR.Round(time.Millisecond))
	fmt.Printf("PER-MR    extrapolated to %d merge requests: %d in tokens, %s of wall clock\n",
		len(mrs), inTok/len(sample)*len(mrs), (perMR * time.Duration(len(mrs))).Round(time.Second))

	// PER-PROJECT: one call over the week's event list.
	var list strings.Builder
	for _, e := range all {
		fmt.Fprintf(&list, "- [%s %s] (%s, by %s) %s (tags: %s)\n",
			e.Kind, e.Ref, e.Action, e.Author, e.Title, strings.Join(e.Labels, ","))
	}
	start = time.Now()
	r, err := client.SystemOne(context.Background(), decide.Request{
		State: decide.State{Body: list.String()}, Questions: questions,
	})
	if err != nil {
		t.Errorf("per-project call: %v", err)
		return
	}
	fmt.Printf("PER-PROJ  1 call over %d bytes: %d in / %d out tokens, %s, dropped=%d truncated=%v\n",
		list.Len(), r.Usage.InputTokens, r.Usage.OutputTokens,
		time.Since(start).Round(time.Millisecond), r.Usage.StateTokensDropped, r.Usage.Truncated)
	fmt.Printf("PER-PROJ  peer_relevant=%v blocked=%v kind=%q\n",
		r.Answers["peer_relevant"].NoulValue(),
		r.Answers["blocked"].NoulValue(),
		r.Answers["kind"].ChoiceKey())
}
