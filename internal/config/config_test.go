package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/triage"
)

// writeFixture drops a config file into a temp dir and returns its path.
func writeFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summarizer.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// TestLoad_EveryDocumentedKeyBinds: the YAML keys shipped in
// examples/summarizer.yaml must actually populate the struct. yaml.v3
// lowercases untagged field names, so a missing tag makes the key a
// silent no-op — gitlab_host: shipped that way and Validate() then
// blamed the very key the operator had set.
func TestLoad_EveryDocumentedKeyBinds(t *testing.T) {
	body := `
gitlab_host: "https://gitlab.example.com"
model: "gpt-4o"
output_dir: "/tmp/out"
max_events: 42
max_tokens: 4096
rollup_total_cap: 32000
rollup_body_cap: 8000
diff_max_per_mr: 8000
max_diff_runes_per_file: 1200
provider: "anthropic"
openai_base_url: "http://localhost:11434/v1"
internal_only_projects:
  - group/secret
publish:
  enabled: true
  host: "https://gitlab.example.com"
  token_env: "TOK"
  project: "ops/reports"
  prefix: "reports"
  default_branch: "trunk"
  source_branch_prefix: "sum"
safety:
  enabled: true
  decision_url: "https://decide.example.com"
  decision_token_env: "DEC"
  injection_threshold: 0.9
triage:
  enabled: true
  decision_url: "https://triage.example.com"
  decision_token_env: "TRIAGE_TOK"
  mode: "mr"
`
	cfg, err := Load(writeFixture(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	strs := []struct{ name, got, want string }{
		{"gitlab_host", cfg.GitLabHost, "https://gitlab.example.com"},
		{"model", cfg.Model, "gpt-4o"},
		{"output_dir", cfg.OutputDir, "/tmp/out"},
		{"provider", cfg.Provider, "anthropic"},
		{"openai_base_url", cfg.OpenAIBaseURL, "http://localhost:11434/v1"},
		{"publish.token_env", cfg.Publish.TokenEnv, "TOK"},
		{"publish.project", cfg.Publish.Project, "ops/reports"},
		{"publish.prefix", cfg.Publish.Prefix, "reports"},
		{"publish.default_branch", cfg.Publish.DefaultBranch, "trunk"},
		{"publish.source_branch_prefix", cfg.Publish.SourceBranchPrefix, "sum"},
		{"safety.decision_url", cfg.Safety.DecisionURL, "https://decide.example.com"},
		{"safety.decision_token_env", cfg.Safety.DecisionTokenEnv, "DEC"},
		{"triage.decision_url", cfg.Triage.DecisionURL, "https://triage.example.com"},
		{"triage.decision_token_env", cfg.Triage.DecisionTokenEnv, "TRIAGE_TOK"},
		{"triage.mode", cfg.Triage.Mode, "mr"},
	}
	for _, c := range strs {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.MaxEvents != 42 {
		t.Errorf("max_events: got %d, want 42", cfg.MaxEvents)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"max_tokens", cfg.MaxTokens, 4096},
		{"rollup_total_cap", cfg.RollupTotalCap, 32000},
		{"rollup_body_cap", cfg.RollupBodyCap, 8000},
		{"diff_max_per_mr", cfg.DiffMaxPerMR, 8000},
		{"max_diff_runes_per_file", cfg.MaxDiffRunesPerFile, 1200},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}
	if cfg.Safety.InjectionThreshold != 0.9 {
		t.Errorf("safety.injection_threshold: got %v, want 0.9", cfg.Safety.InjectionThreshold)
	}
	if !cfg.Publish.Enabled || !cfg.Safety.Enabled || !cfg.Triage.Enabled {
		t.Errorf("publish.enabled=%v safety.enabled=%v triage.enabled=%v, want all true",
			cfg.Publish.Enabled, cfg.Safety.Enabled, cfg.Triage.Enabled)
	}
	if len(cfg.InternalOnlyProjects) != 1 || cfg.InternalOnlyProjects[0] != "group/secret" {
		t.Errorf("internal_only_projects: got %v", cfg.InternalOnlyProjects)
	}
}

// TestLoad_MissingFileIsNotAnError: the default config path almost
// never exists; that must be a zero Config, not a failure.
func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("missing file should yield a zero Config, not nil")
	}
	if cfg.GitLabHost != "" {
		t.Errorf("zero Config should have empty host, got %q", cfg.GitLabHost)
	}
}

// TestLoad_MalformedYAMLErrors: a typo in the file must fail loudly at
// load time rather than silently dropping the operator's settings.
func TestLoad_MalformedYAMLErrors(t *testing.T) {
	if _, err := Load(writeFixture(t, "gitlab_host: [unclosed\n")); err == nil {
		t.Fatal("malformed YAML should error")
	}
}

// TestDefaults: zero values get the documented built-ins.
func TestDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults()
	if cfg.Model != "gpt-4o-mini" {
		t.Errorf("Model: got %q", cfg.Model)
	}
	if cfg.OutputDir != "./summarizer-out" {
		t.Errorf("OutputDir: got %q", cfg.OutputDir)
	}
	if cfg.MaxEvents != 200 {
		t.Errorf("MaxEvents: got %d", cfg.MaxEvents)
	}
	if cfg.Provider != "openai" {
		t.Errorf("Provider: got %q", cfg.Provider)
	}
	if cfg.OpenAIBaseURL != "https://api.openai.com/v1" {
		t.Errorf("OpenAIBaseURL: got %q", cfg.OpenAIBaseURL)
	}
	// Defaults must not clobber operator values.
	set := &Config{Model: "custom", MaxEvents: 7}
	set.Defaults()
	if set.Model != "custom" || set.MaxEvents != 7 {
		t.Errorf("Defaults overwrote explicit values: %+v", set)
	}
}

// TestDefaults_AnthropicGetsItsOwnModel: the model default is
// per-provider. One default for both meant `--provider anthropic` on a
// config with no `model:` posted gpt-4o-mini to api.anthropic.com,
// which answers 404 model_not_found — and the README offers that flag
// as a headline example.
func TestDefaults_AnthropicGetsItsOwnModel(t *testing.T) {
	// Order matters and is what cmd/ does: the provider override lands
	// before Defaults(), so Defaults() must read the resolved provider.
	cfg := &Config{GitLabHost: "https://gitlab.example.com", Provider: "anthropic"}
	cfg.Defaults()
	if cfg.Model != "claude-3-5-haiku-latest" {
		t.Errorf("anthropic default model: got %q, want claude-3-5-haiku-latest", cfg.Model)
	}
	// An explicit model still wins over the provider default.
	pinned := &Config{Provider: "anthropic", Model: "claude-sonnet-4-5"}
	pinned.Defaults()
	if pinned.Model != "claude-sonnet-4-5" {
		t.Errorf("Defaults overwrote an explicit model: %q", pinned.Model)
	}
}

// TestValidate: the required-host check, the provider enum, and the
// MaxEvents floor.
func TestValidate(t *testing.T) {
	base := func() *Config {
		c := &Config{GitLabHost: "https://gitlab.example.com"}
		c.Defaults()
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name  string
		mutit func(*Config)
	}{
		{"no host", func(c *Config) { c.GitLabHost = "" }},
		{"bad provider", func(c *Config) { c.Provider = "gemini" }},
		{"max_events zero", func(c *Config) { c.MaxEvents = 0 }},
		{"max_events negative", func(c *Config) { c.MaxEvents = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutit(c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected an error, got nil (%+v)", c)
			}
		})
	}
}

// TestValidate_TriageMode: the two triage call shapes differ in cost
// by two orders of magnitude, so a misspelled mode must not silently
// pick one of them — the operator would be running the expensive shape
// and billing for it without knowing.
//
// An empty mode is the default (week) and is fine.
func TestValidate_TriageMode(t *testing.T) {
	base := func() *Config {
		c := &Config{GitLabHost: "https://gitlab.example.com", Triage: TriageConfig{Enabled: true}}
		c.Defaults()
		return c
	}
	for _, mode := range []string{"", triage.ModeWeek, triage.ModeMR} {
		c := base()
		c.Triage.Mode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	for _, mode := range []string{"per-event", "project", "MR", "Week"} {
		c := base()
		c.Triage.Mode = mode
		if err := c.Validate(); err == nil {
			t.Errorf("mode %q accepted; only %q and %q are real", mode, triage.ModeWeek, triage.ModeMR)
		}
	}
	// A bad mode with triage switched off is nobody's problem: the
	// field is never read.
	c := base()
	c.Triage.Enabled = false
	c.Triage.Mode = "per-event"
	if err := c.Validate(); err != nil {
		t.Errorf("a disabled triage must not reject the config: %v", err)
	}
}

// TestPublishTarget: the reports project is skipped by discovery, but
// only when it is a project the scan can actually see. A cross-host
// reports repo is invisible to the source scan anyway, and matching on
// path alone would exclude an unrelated same-named project.
func TestPublishTarget(t *testing.T) {
	const host = "https://gitlab.example.com"
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"local publisher", Config{GitLabHost: host}, ""},
		{"publish disabled", Config{GitLabHost: host,
			Publish: PublishConfig{Project: "sfb/reports"}}, ""},
		{"no project", Config{GitLabHost: host,
			Publish: PublishConfig{Enabled: true}}, ""},
		{"same host by default", Config{GitLabHost: host,
			Publish: PublishConfig{Enabled: true, Project: "sfb/reports"}}, "sfb/reports"},
		{"explicit same host", Config{GitLabHost: host,
			Publish: PublishConfig{Enabled: true, Host: host, Project: "sfb/reports"}}, "sfb/reports"},
		{"cross-host", Config{GitLabHost: host,
			Publish: PublishConfig{Enabled: true, Host: "https://reports.example.net",
				Project: "sfb/reports"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.PublishTarget(); got != tc.want {
				t.Errorf("PublishTarget() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidate_TrimsInternalOnlyProjects: the --internal-only-project
// flag trims each value; the YAML block used not to. A list pasted out
// of a spreadsheet therefore behaved differently from the same list
// typed as a flag, and "  group/secret  " could never match a project
// path — which is an audience control failing open, not a cosmetic
// difference.
func TestValidate_TrimsInternalOnlyProjects(t *testing.T) {
	cfg := &Config{
		GitLabHost: "https://gitlab.example.com",
		Provider:   "openai",
		MaxEvents:  200,
		InternalOnlyProjects: []string{
			"  group/secret  ",
			"\tgroup/other\t",
			"   ", // whitespace only: no project, drop it
			"group/third",
		},
	}
	cfg.Defaults() // Validate runs after Defaults: it fills the budgets.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []string{"group/secret", "group/other", "group/third"}
	if len(cfg.InternalOnlyProjects) != len(want) {
		t.Fatalf("got %q, want %q", cfg.InternalOnlyProjects, want)
	}
	for i, w := range want {
		if cfg.InternalOnlyProjects[i] != w {
			t.Errorf("entry %d = %q, want %q", i, cfg.InternalOnlyProjects[i], w)
		}
	}
}

// TestDefaults_Caps: every budget has a documented default, and the
// default is the one the code used before the budget was configurable.
// A default that drifts from the old constant changes every report
// nobody asked to change.
func TestDefaults_Caps(t *testing.T) {
	c := &Config{GitLabHost: "https://gitlab.example.com"}
	c.Defaults()
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"max_events", c.MaxEvents, 200},
		{"max_tokens", c.MaxTokens, llm.DefaultMaxTokens},
		{"rollup_total_cap", c.RollupTotalCap, prompt.DefaultRollupTotal},
		{"rollup_body_cap", c.RollupBodyCap, prompt.DefaultRollupPerBody},
		{"diff_max_per_mr", c.DiffMaxPerMR, prompt.DefaultDiffMaxPerMR},
		{"max_diff_runes_per_file", c.MaxDiffRunesPerFile, gitlab.DefaultMaxDiffRunesPerFile},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// The completion cap was 2048 and was reachable on a busy week.
	if llm.DefaultMaxTokens != 4096 {
		t.Errorf("llm.DefaultMaxTokens = %d, want 4096", llm.DefaultMaxTokens)
	}
}

// TestValidate_NegativeCapIsRejected: a budget of zero means "send
// nothing" — an empty prompt, or a diff truncated to nothing. A
// negative number is a typo, and Defaults() has already turned an
// unset zero into the documented default, so a negative is all that is
// left to catch.
func TestValidate_NegativeCapIsRejected(t *testing.T) {
	base := func() *Config {
		c := &Config{GitLabHost: "https://gitlab.example.com"}
		c.Defaults()
		return c
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"max_tokens", func(c *Config) { c.MaxTokens = -1 }},
		{"rollup_total_cap", func(c *Config) { c.RollupTotalCap = -1 }},
		{"rollup_body_cap", func(c *Config) { c.RollupBodyCap = -1 }},
		{"diff_max_per_mr", func(c *Config) { c.DiffMaxPerMR = -1 }},
		{"max_diff_runes_per_file", func(c *Config) { c.MaxDiffRunesPerFile = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(c)
			if err := c.Validate(); err == nil {
				t.Errorf("a negative %s was accepted", tc.name)
			}
		})
	}
}

// TestLimits_MapsEveryCap: the config's keys and the prompt package's
// struct are two names for the same budgets. A key that is not mapped
// would leave the prompt builder on its default while the operator
// believes they changed it.
func TestLimits_MapsEveryCap(t *testing.T) {
	c := &Config{
		GitLabHost:          "https://gitlab.example.com",
		RollupBodyCap:       111,
		RollupTotalCap:      222,
		DiffMaxPerMR:        333,
		MaxDiffRunesPerFile: 444,
	}
	c.Defaults()
	lim := c.Limits()
	if lim.RollupPerBody != 111 || lim.RollupTotal != 222 || lim.DiffPerMR != 333 {
		t.Errorf("Limits() = %+v, want the config's own numbers", lim)
	}
}

// TestValidate_NilListStaysNil: a config with no internal_only_projects
// block must not turn into an empty non-nil slice, so "unset" stays
// distinguishable from "set to nothing" the way CustomerCodes does.
func TestValidate_NilListStaysNil(t *testing.T) {
	cfg := &Config{GitLabHost: "https://gitlab.example.com", Provider: "openai", MaxEvents: 200}
	// Defaults() first: Validate is documented as running after it, and
	// it is what fills the budget defaults. Without it the caps are
	// still zero and Validate correctly refuses them.
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.InternalOnlyProjects != nil {
		t.Errorf("unset list became %q", cfg.InternalOnlyProjects)
	}
}
