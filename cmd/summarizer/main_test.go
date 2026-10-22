package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/run"
	"github.com/inful/summarizer/internal/safety"
	"gopkg.in/yaml.v3"
)

// TestUnionSlices_DedupesAndKeepsOrder: --internal-only-project and the
// YAML block are unioned; the first occurrence wins and order is stable
// so the run manifest reads predictably.
func TestUnionSlices_DedupesAndKeepsOrder(t *testing.T) {
	got := unionSlices([]string{"a", "b"}, []string{"b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(unionSlices(nil, nil)) != 0 {
		t.Error("two nil inputs should yield an empty slice, not nil-ish garbage")
	}
}

// TestClassify_TypedSentinelsWin: the sentinel we can match reliably
// must NOT depend on the message text. ErrInjection carries the word
// "write"-adjacent vocabulary in some wrappings, but a wrapped
// error's outer text is what classify sees.
func TestClassify_TypedSentinelsWin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"injection", fmt.Errorf("project g/p: %w", run.ErrInjection), exitInjection},
		{"gitlab", errors.New("fetch mrs: 500 boom"), exitGitLab},
		{"llm", errors.New("llm customer: connection reset"), exitLLM},
		{"write", errors.New("write team: disk full"), exitWrite},
		{"config", errors.New("config: bad host"), exitConfig},
		{"render", errors.New("render customer: template broke"), exitLLM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Errorf("classify(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestClassify_UnclassifiedIsDocumented: an unrecognised failure must
// return the documented exitUnknown code, not a bare literal, so the
// README table stays honest.
func TestClassify_UnclassifiedIsDocumented(t *testing.T) {
	got := classify(errors.New("something nobody predicted"))
	if got != exitUnknown {
		t.Errorf("classify = %d, want exitUnknown (%d)", got, exitUnknown)
	}
	if exitUnknown != 1 {
		t.Errorf("exitUnknown = %d; README documents 1", exitUnknown)
	}
}

// TestPrintSummary_CountsFilesActuallyWritten: the summary said
// "2 * len(projects)" plus rollups, which over-counted every team-only
// project (one file, not two) and ignored the epic rollups entirely.
// An operator reconciling the target repo against this line could not.
func TestPrintSummary_CountsFilesActuallyWritten(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{
			{Project: "a", EventsFetched: 10, EventsUsed: 10, FilesWritten: []string{"a.md"}, CustomerOn: true},
			{Project: "b", EventsFetched: 5, EventsUsed: 5, FilesWritten: []string{"b.md"}, TeamOnly: true},
			{Project: "c", EventsFetched: 7, EventsUsed: 7, FilesWritten: []string{"c.md"}}, // 2 files
		},
		RollupFiles:     []string{"r1", "r2"},
		EpicRollupFiles: []string{"e1"},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if !strings.Contains(out, "6 file(s) written") {
		t.Errorf("want 6 files (3 per-project + 2 rollups + 1 epic rollup), got:\n%s", out)
	}
	if !strings.Contains(out, "e1 (epics rollup)") {
		t.Errorf("epic rollup files should be listed alongside the others:\n%s", out)
	}
	if !strings.Contains(out, "22 event(s)") {
		t.Errorf("event totals wrong:\n%s", out)
	}
}

// TestPrintSummary_NoFileClaimOnDryRun: a dry run writes nothing, so it
// must not claim it did.
func TestPrintSummary_NoFileClaimOnDryRun(t *testing.T) {
	res := run.Result{Projects: []run.ProjectResult{{Project: "a", FilesWritten: []string{"a.md"}}}}
	out := captureStderr(t, func() { printSummary(res, nil, true, time.Now()) })
	if strings.Contains(out, "file(s) written") {
		t.Errorf("dry run must not claim writes:\n%s", out)
	}
}

// TestPrintSummary_SurfacesTheError: the run's stderr line is the only
// place the operator sees what failed.
func TestPrintSummary_SurfacesTheError(t *testing.T) {
	out := captureStderr(t, func() {
		printSummary(run.Result{}, errors.New("probe g/p: 403 forbidden"), false, time.Now())
	})
	if !strings.Contains(out, "403 forbidden") {
		t.Errorf("error not surfaced:\n%s", out)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// what was written. printSummary writes straight to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

// TestConfigExampleIsLoadable: examples/summarizer.yaml is the file the
// README tells operators to copy. It must parse into the struct, and
// every key it uses must be one the loader binds.
func TestConfigExampleIsLoadable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "summarizer.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	// Unknown-field strictness is the whole point: yaml.Unmarshal is
	// permissive by default, which is how a key that binds to nothing
	// ships unnoticed.
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	cfg := &config.Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		t.Errorf("example config has keys the loader does not bind: %v", err)
	}
	if cfg.GitLabHost == "" {
		t.Error("example config sets gitlab_host but the loader did not bind it")
	}
}

// TestPrintSummary_CountsAlreadyPublishedSeparately: re-running a week
// whose MR was already merged writes the file onto a branch that will
// never merge again. The publisher sets Result.Closed for those, and
// the summary has to say so — "ok — 4 file(s) written" for a run that
// published nothing to the default branch is the exact confusion the
// no-op was introduced to remove.
func TestPrintSummary_CountsAlreadyPublishedSeparately(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{
			// Both files landed, but on a merged branch.
			{Project: "a", EventsFetched: 10, EventsUsed: 10, CustomerOn: true,
				FilesWritten: []string{"a-customer.md", "a-team.md"}, FilesClosed: 2},
		},
		RollupFiles: []string{"r1", "r2"},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if strings.Contains(out, "4 file(s) written (") || strings.Contains(out, "4 file(s) written\n") {
		t.Errorf("a fully re-published run must not claim 4 fresh writes:\n%s", out)
	}
	if !strings.Contains(out, "2 file(s) written") {
		t.Errorf("want 2 files counted as published:\n%s", out)
	}
	if !strings.Contains(out, "2 already published this week") {
		t.Errorf("the re-published files must be named as such:\n%s", out)
	}
}

// TestPrintSummary_NoAlreadyPublishedNoteWhenNoneWereClosed: the note
// only appears when it is true, so a normal run reads normally.
func TestPrintSummary_NoAlreadyPublishedNoteWhenNoneWereClosed(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{
			{Project: "a", EventsFetched: 1, EventsUsed: 1, FilesWritten: []string{"a.md"}},
		},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if strings.Contains(out, "already published") {
		t.Errorf("a normal run should not carry the note:\n%s", out)
	}
	if !strings.Contains(out, "1 file(s) written") {
		t.Errorf("want 1 file:\n%s", out)
	}
}

// TestClassify_MonthlyErrorShapesGetRealCodes: `summarizer monthly`
// used to return exitGitLab for every failure, so an LLM outage and a
// prompt-injection rejection both looked like a GitLab problem to a cron
// wrapper. The documented table (README) promises distinct codes, and a
// wrapper that alerts on 5 or 8 has to receive them.
func TestClassify_MonthlyErrorShapesGetRealCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"list branches failed", errors.New("fetch branches: 502 Bad Gateway"), exitGitLab},
		{"llm failed", errors.New("llm monthly customer: 429 rate limited"), exitLLM},
		{"publish failed", errors.New("write monthly team: 500 oops"), exitWrite},
		{"template failed", errors.New("render monthly rollup customer: bad template"), exitLLM},
		{"bad --month", errors.New(`month "2026-13": want YYYY-MM`), exitConfig},
		{"no weeks published", errors.New("no weekly rollups found for 2026-09"), exitUnknown},
		// The upstream response body is inside the matched string, and
		// every OpenAI-compatible server the README supports sits
		// behind a proxy whose error bodies say "fetch".
		{"llm gateway mentioning fetch", errors.New(`project g/p: llm team: 502 {"error":"failed to fetch upstream model"}`), exitLLM},
		{"injection wins over the string match", fmt.Errorf("%w (score=0.910): monthly-rollup", run.ErrInjection), exitInjection},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("%s: classify(%v) = %d, want %d", c.name, c.err, got, c.want)
		}
	}
}

// TestPrintSummary_FailedRunNeverSaysOK: a run that failed printed a
// PARTIAL line and then an "ok —" line underneath it. The exit code was
// non-zero, but the one word a cron wrapper greps for was right there
// on a run that published less than it claimed.
func TestPrintSummary_FailedRunNeverSaysOK(t *testing.T) {
	res := run.Result{Projects: []run.ProjectResult{{
		Project:      "g/aaa",
		FilesWritten: []string{"/out/2026-W43/g_aaa--wk--team.md"},
	}}}
	out := captureStderr(t, func() {
		printSummary(res, errors.New("project g/zzz: llm team: 500"), false, time.Now())
	})
	if strings.Contains(out, "summarizer: ok") {
		t.Errorf("a failed run printed a line beginning \"summarizer: ok\":\n%s", out)
	}
	if !strings.Contains(out, "summarizer: partial") {
		t.Errorf("the summary should say partial, not ok:\n%s", out)
	}
}

// TestPrintSummary_ListsFilesFromAProjectThatThenFailed: the customer
// report lands, the team call 500s, and the project is dropped from
// res.Projects so it is not counted. The file is still on the branch —
// the summary has to name it or the operator has no way to know.
func TestPrintSummary_ListsFilesFromAProjectThatThenFailed(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{{
			Project:      "g/aaa",
			FilesWritten: []string{"/out/2026-W43/g_aaa--wk--team.md"},
		}},
		PartialFiles: []string{"/out/2026-W43/g_zzz--wk--customer.md"},
	}
	out := captureStderr(t, func() {
		printSummary(res, errors.New("project g/zzz: llm team: 500"), false, time.Now())
	})
	if !strings.Contains(out, "g_zzz--wk--customer.md") {
		t.Errorf("the file a failed project did publish must be listed:\n%s", out)
	}
}

// TestResolveLLM_ProviderDrivesTheModelAndTheKeyRule: resolveLLM is
// the boundary where a config becomes an API adapter, and nothing
// tested it — the thirteenth audit's per-provider model default landed
// in internal/config with nothing pinning that the adapter receives the
// right thing. It receives cfg.Model, and the empty-key rule is
// per-host.
func TestResolveLLM_ProviderDrivesTheModelAndTheKeyRule(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	t.Run("anthropic gets the anthropic default model", func(t *testing.T) {
		cfg := &config.Config{Provider: "anthropic", GitLabHost: "https://x"}
		cfg.Defaults()
		p, code := resolveLLM(cfg, "", "sk-ant-test")
		if code != exitOK {
			t.Fatalf("code = %d", code)
		}
		if got := p.(*llm.AnthropicProvider).Model; got != "claude-3-5-haiku-latest" {
			t.Errorf("anthropic model = %q, want claude-3-5-haiku-latest (an OpenAI id here is a 404 from the API)", got)
		}
	})

	t.Run("api.openai.com still requires a key", func(t *testing.T) {
		cfg := &config.Config{Provider: "openai", GitLabHost: "https://x"}
		cfg.Defaults()
		if _, code := resolveLLM(cfg, "", ""); code != exitEnv {
			t.Errorf("code = %d, want %d (exitEnv) — an unauthenticated run against api.openai.com", code, exitEnv)
		}
	})

	t.Run("a local server does not", func(t *testing.T) {
		cfg := &config.Config{Provider: "openai", GitLabHost: "https://x", OpenAIBaseURL: "http://localhost:11434/v1"}
		cfg.Defaults()
		p, code := resolveLLM(cfg, "", "")
		if code != exitOK {
			t.Fatalf("code = %d, want 0 — Ollama and friends ignore an empty key", code)
		}
		if got := p.(*llm.OpenAIProvider).APIKey; got != "" {
			t.Errorf("APIKey = %q, want empty", got)
		}
	})

	t.Run("unknown provider is a config error", func(t *testing.T) {
		cfg := &config.Config{Provider: "ollama", GitLabHost: "https://x"}
		cfg.Defaults()
		if _, code := resolveLLM(cfg, "k", ""); code != exitConfig {
			t.Errorf("code = %d, want %d", code, exitConfig)
		}
	})
}

// TestNewGuard_UntouchedConfigIsANoop: safety.enabled is opt-in, and
// the guard must not fire for a config that never mentions it. The
// decision was getting this wrong is a report that is silently
// un-guarded while the operator believes otherwise — the guard
// soft-fails on transport errors too, so the only visible signal is
// the absence of its log lines.
func TestNewGuard_UntouchedConfigIsANoop(t *testing.T) {
	cfg := &config.Config{GitLabHost: "https://x"}
	cfg.Defaults()
	g, code := newGuard(cfg, false, "", "")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if _, ok := g.(safety.NoopGuard); !ok {
		t.Errorf("guard = %T, want safety.NoopGuard when neither the flag nor the config enables it", g)
	}

	// The flag alone is enough, and needs a URL.
	if _, code := newGuard(cfg, true, "", ""); code != exitConfig {
		t.Errorf("code = %d, want %d — --safety-enabled with no decision URL", code, exitConfig)
	}
	if g, code := newGuard(cfg, true, "https://decide.example", ""); code != exitOK || g == nil {
		t.Errorf("--safety-enabled with a URL should build a guard, got %T code=%d", g, code)
	}
}

// TestNewPublisher_PublishNeedsItsTokenAndProject: the two failure
// modes newPublisher reports, in order. They have distinct exit codes,
// so the order is load-bearing for a cron wrapper: exit 7 for a
// missing env var, exit 3 for a malformed config.
func TestNewPublisher_PublishNeedsItsTokenAndProject(t *testing.T) {
	t.Setenv("GITLAB_REPORTS_TOKEN", "")

	plain := &config.Config{GitLabHost: "https://x", OutputDir: t.TempDir()}
	plain.Defaults()
	pub, code := newPublisher(plain)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if _, ok := pub.(*publisher.Local); !ok {
		t.Errorf("publish disabled must give the local publisher, got %T", pub)
	}

	noToken := &config.Config{GitLabHost: "https://x", Publish: config.PublishConfig{Enabled: true, Project: "g/reports"}}
	if _, code := newPublisher(noToken); code != exitEnv {
		t.Errorf("code = %d, want %d (exitEnv) — publish enabled with no token", code, exitEnv)
	}

	// The token is checked before the project, so it has to be present
	// to reach the second check.
	t.Setenv("GITLAB_REPORTS_TOKEN", "glpat-test")
	noProject := &config.Config{GitLabHost: "https://x", Publish: config.PublishConfig{Enabled: true}}
	if _, code := newPublisher(noProject); code != exitConfig {
		t.Errorf("code = %d, want %d (exitConfig) — publish enabled with no target project", code, exitConfig)
	}

	ok := &config.Config{GitLabHost: "https://x", Publish: config.PublishConfig{Enabled: true, Project: "g/reports"}}
	if _, code := newPublisher(ok); code != exitOK {
		t.Errorf("code = %d, want 0", code)
	}
}

// TestPrintSummary_DiscountsRollupsThatReachedNoBranch: a re-run of a
// week whose MR is already merged writes every file — per-project and
// rollup alike — onto a branch that will never merge again. The
// per-project path reported that; the rollup path discarded
// Result.Closed, so the summary said files were published when nothing
// had reached main.
func TestPrintSummary_DiscountsRollupsThatReachedNoBranch(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{{
			Project: "g/aaa", FilesWritten: []string{"a-team.md"}, FilesClosed: 1,
		}},
		RollupFiles:       []string{"combined--rollup--wk--team.md"},
		RollupFilesClosed: 1,
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if !strings.Contains(out, "0 file(s) written") {
		t.Errorf("two files were written and both landed on a closed branch, so "+
			"nothing reached main:\n%s", out)
	}
	if !strings.Contains(out, "(2 already published this week)") {
		t.Errorf("both writes should be discounted, the rollup included:\n%s", out)
	}
}

// TestPrintSummary_ReportsAFilesEvenWhenEveryProjectFailed: a run
// where every project published and then failed has an empty
// res.Projects, and gating on that alone printed nothing — the files
// were on the branch and nothing named them. The partial file must
// appear AND be counted.
func TestPrintSummary_ReportsAFilesEvenWhenEveryProjectFailed(t *testing.T) {
	res := run.Result{PartialFiles: []string{"/out/2026-W43/g_zzz--wk--customer.md"}}
	out := captureStderr(t, func() {
		printSummary(res, errors.New("project g/zzz: llm team: 500"), false, time.Now())
	})
	if !strings.Contains(out, "g_zzz--wk--customer.md") {
		t.Errorf("the file a failed project did publish must be named:\n%s", out)
	}
	if !strings.Contains(out, "1 file(s) written") {
		t.Errorf("a file listed in the output must also be counted:\n%s", out)
	}
}
