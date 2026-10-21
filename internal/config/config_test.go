package config

import (
	"os"
	"path/filepath"
	"testing"
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
	}
	for _, c := range strs {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.MaxEvents != 42 {
		t.Errorf("max_events: got %d, want 42", cfg.MaxEvents)
	}
	if cfg.Safety.InjectionThreshold != 0.9 {
		t.Errorf("safety.injection_threshold: got %v, want 0.9", cfg.Safety.InjectionThreshold)
	}
	if !cfg.Publish.Enabled || !cfg.Safety.Enabled {
		t.Errorf("publish.enabled=%v safety.enabled=%v, want both true", cfg.Publish.Enabled, cfg.Safety.Enabled)
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
