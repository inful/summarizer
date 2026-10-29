// Package config loads summarizer configuration from a YAML file, with env
// vars and CLI flags resolved at the boundary in cmd/summarizer. The point
// of this package is one struct, one Load() function, and one Validate().
//
// Resolution precedence (highest wins):
//  1. CLI flag (applied by main before Load is called)
//  2. Env var
//  3. YAML file
//  4. Built-in default
//
// This package imports the packages that ENFORCE the budgets (prompt,
// llm, gitlab, triage) so that each default has exactly one home: the
// package that applies it. The alternative is a second copy of every
// number, and two copies of a cap are two answers to "what is the
// limit". The cost is that config can no longer be imported by those
// packages without a cycle — which is the price of the single copy, and
// is worth it. Adding a fifth import for the same reason is fine; adding
// one for a different reason is not.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/triage"
)

// Config is the resolved summarizer configuration for one run.
type Config struct {
	// Required: GitLab host (e.g. "https://gitlab.example.com"). Has no
	// default — refusing to start without it is the correct behavior.
	GitLabHost string `yaml:"gitlab_host"`

	// Per-project default model for the chosen provider. Default: gpt-4o-mini.
	// Provider-specific: an Anthropic model id if provider="anthropic".
	Model string `yaml:"model"`

	// Output directory for the two reports. Default: ./summarizer-out.
	OutputDir string `yaml:"output_dir"`

	// Hard cap on events sent to the LLM. Default 200. Excess is dropped
	// (most-recent-wins) and the truncation is disclosed in the prompt.
	MaxEvents int `yaml:"max_events"`

	// MaxTokens caps the completion the LLM may produce, per call.
	// Default 4096. A response that hits the cap is detected by both
	// providers and fails the run rather than publishing a fragment —
	// so this is the number to raise when a report is being cut off
	// mid-sentence.
	MaxTokens int `yaml:"max_tokens"`

	// RollupTotalCap caps the whole cross-project rollup prompt's
	// project text, in bytes. Default 16000. When it bites, the
	// least-active projects are dropped from the rollup.
	RollupTotalCap int `yaml:"rollup_total_cap"`

	// RollupBodyCap caps one project's report text inside a rollup
	// prompt, in runes. Default 4000.
	RollupBodyCap int `yaml:"rollup_body_cap"`

	// DiffMaxPerMR caps the total diff text per merge request in the
	// team prompt, in bytes. Default 4000. Only applies with
	// --include-diffs.
	DiffMaxPerMR int `yaml:"diff_max_per_mr"`

	// MaxDiffRunesPerFile caps the per-file diff body the GitLab
	// client retains in memory, in runes. Default 600.
	MaxDiffRunesPerFile int `yaml:"max_diff_runes_per_file"`

	// Per-project default provider: "openai" or "anthropic". CLI flag overrides.
	Provider string `yaml:"provider"`

	// OpenAIBaseURL overrides the OpenAI-compatible endpoint. Default
	// points at api.openai.com; for local servers set to e.g.
	// "http://localhost:11434/v1" (Ollama), "http://localhost:1234/v1"
	// (LM Studio), or "http://localhost:8000/v1" (vLLM). The URL
	// normalizer accepts both bare-host and /v1-suffixed forms.
	OpenAIBaseURL string `yaml:"openai_base_url"`

	// InternalOnlyProjects is the operator-curated list of projects
	// that should never produce customer-facing output. Same effect
	// as the GitLab-side `summarize-team-only` topic (D5) or the
	// --internal-only-project CLI flag — the three sources union
	// together. See internal/policy for the classifier.
	//
	// This is an audience switch, not an opt-out: a listed project
	// still runs and still gets a team report. To skip a project
	// entirely, use its `no-summarize` topic or a .summaryignore file
	// in the project itself.
	//
	// There is deliberately no `projects:` list here: the project set
	// comes from --project or, by default, --discover-active.
	// --discover-group and --discover-by-topic narrow that. A
	// config-file project list would be a second, silently-ignored
	// way to say the same thing.
	InternalOnlyProjects []string `yaml:"internal_only_projects"`

	// Publish configures the optional GitLab MR-based publisher.
	// When enabled, the local disk writer is skipped and every report
	// file is committed to a per-period branch with a reviewable MR
	// against the configured default branch.
	Publish PublishConfig `yaml:"publish"`

	// Safety gates LLM calls behind a prompt-injection detector.
	// Off by default; opt in with safety.enabled=true.
	Safety SafetyConfig `yaml:"safety"`

	// Triage classifies each project's activity with the decision API
	// before the prompt is rendered. Off by default; opt in with
	// triage.enabled=true.
	Triage TriageConfig `yaml:"triage"`
}

// PublishConfig is the GitLab-MR publisher configuration. All-zero value
// means "use the local disk writer"; a non-zero Enabled implies the rest
// of the fields must be set.
type PublishConfig struct {
	Enabled            bool   `yaml:"enabled"`              // when true, use GitLab MR publisher
	Host               string `yaml:"host"`                 // API base URL (default = same as GitLab host)
	TokenEnv           string `yaml:"token_env"`            // env var name holding a PAT with api scope on the target project; default "GITLAB_REPORTS_TOKEN"
	Project            string `yaml:"project"`              // target project path on the same host
	Prefix             string `yaml:"prefix"`               // repo path prefix for reports (e.g. "reports"); "" = repo root
	DefaultBranch      string `yaml:"default_branch"`       // MR target; default "main"
	SourceBranchPrefix string `yaml:"source_branch_prefix"` // e.g. "summarizer"; branch = "<prefix>/<period>"
}

// SafetyConfig gates LLM calls behind a prompt-injection detector. Opt-in
// (Enabled=false means no check). When enabled, every rendered prompt is
// sent to a decision API (inful/decide) before the LLM call; noul above
// the threshold = injection = hard-fail the run.
type SafetyConfig struct {
	Enabled            bool    `yaml:"enabled"`             // when true, every LLM call is gated
	DecisionURL        string  `yaml:"decision_url"`        // base URL of the /v1/systemone API
	DecisionTokenEnv   string  `yaml:"decision_token_env"`  // env var holding the API token; default "SAFETY_DECISION_TOKEN"
	InjectionThreshold float64 `yaml:"injection_threshold"` // noul above this = injection; default 0.7
}

// TriageConfig classifies a project's week of activity with the decision
// API before the prompt is rendered. Opt-in (Enabled=false means no
// classification, and the prompt asks about nothing the data cannot
// answer).
//
// The classifier answers questions ABOUT a period, not about an item: a
// noul is one probability for the whole state. So it can say "there is
// blocked work this week" and never "MR #7 is blocked" — the report's
// model, which sees every event, says which one. See internal/triage.
type TriageConfig struct {
	Enabled bool `yaml:"enabled"` // when true, every project is classified before its prompt is rendered
	// DecisionURL is the base URL of the /v1/systemone API. Empty
	// falls back to safety.decision_url — it is the same service.
	DecisionURL string `yaml:"decision_url"`
	// DecisionTokenEnv names the env var holding the API token. Empty
	// falls back to safety.decision_token_env.
	DecisionTokenEnv string `yaml:"decision_token_env"`
	// Mode selects the call shape: "week" (default) makes one call per
	// project over its whole event list; "mr" makes one call per merge
	// request, which is the only shape that can answer per item and
	// the only one whose cost scales with the size of the week.
	Mode string `yaml:"mode"`
}

// Limits returns the prompt-builder budgets this config asks for.
//
// It is the one place the config's individual keys are mapped onto the
// prompt package's struct, so the mapping is stated once instead of at
// every call site.
func (c *Config) Limits() prompt.Limits {
	return prompt.Limits{
		RollupPerBody: c.RollupBodyCap,
		RollupTotal:   c.RollupTotalCap,
		DiffPerMR:     c.DiffMaxPerMR,
	}
}

// PublishTarget is the project summarizer writes its reports into, or
// "" when there is none to skip.
//
// A reports repo is not a project anyone wants summarised: its MRs are
// summarizer's own publishing MRs, so including it would spend an LLM
// call every week describing the previous week's reports. Discovery
// skips it by default.
//
// Only returned when the publisher lives on the SAME host. A
// cross-host reports repo is invisible to the source scan anyway, and
// matching on path alone would exclude an unrelated same-named project
// on the source host.
func (c *Config) PublishTarget() string {
	if !c.Publish.Enabled || c.Publish.Project == "" {
		return ""
	}
	if h := c.Publish.Host; h != "" && h != c.GitLabHost {
		return ""
	}
	return c.Publish.Project
}

// Defaults fills in zero values with sensible built-ins.
func (c *Config) Defaults() {
	if c.Model == "" {
		// Per provider. One model for both is a lie the API rejects:
		// the OpenAI default posted to api.anthropic.com is a 404
		// model_not_found, and the README offers `--provider
		// anthropic` as a headline example.
		if c.Provider == "anthropic" {
			c.Model = "claude-3-5-haiku-latest"
		} else {
			c.Model = "gpt-4o-mini"
		}
	}
	if c.OutputDir == "" {
		c.OutputDir = "./summarizer-out"
	}
	if c.MaxEvents == 0 {
		c.MaxEvents = 200
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = llm.DefaultMaxTokens
	}
	if c.RollupTotalCap == 0 {
		c.RollupTotalCap = prompt.DefaultRollupTotal
	}
	if c.RollupBodyCap == 0 {
		c.RollupBodyCap = prompt.DefaultRollupPerBody
	}
	if c.DiffMaxPerMR == 0 {
		c.DiffMaxPerMR = prompt.DefaultDiffMaxPerMR
	}
	if c.MaxDiffRunesPerFile == 0 {
		c.MaxDiffRunesPerFile = gitlab.DefaultMaxDiffRunesPerFile
	}
	if c.Provider == "" {
		c.Provider = "openai"
	}
	if c.OpenAIBaseURL == "" {
		c.OpenAIBaseURL = "https://api.openai.com/v1"
	}
}

// Validate returns an error for any missing or malformed field. Call after
// Defaults() and after CLI overrides are applied.
func (c *Config) Validate() error {
	if c.GitLabHost == "" {
		return errors.New("config: gitlab host is required (set gitlab_host in config or --gitlab-host on CLI)")
	}
	if c.Provider != "openai" && c.Provider != "anthropic" {
		return fmt.Errorf("config: provider must be openai or anthropic, got %q", c.Provider)
	}
	if c.MaxEvents < 1 {
		return fmt.Errorf("config: max_events must be ≥ 1, got %d", c.MaxEvents)
	}
	// Every cap is a budget, and a budget of zero means "send nothing" —
	// an empty prompt or a truncated-to-nothing diff. Defaults() has
	// already replaced a zero with the documented default, so anything
	// still ≤ 0 here was set to a negative number by the operator.
	for _, cap := range []struct {
		name string
		val  int
	}{
		{"max_tokens", c.MaxTokens},
		{"rollup_total_cap", c.RollupTotalCap},
		{"rollup_body_cap", c.RollupBodyCap},
		{"diff_max_per_mr", c.DiffMaxPerMR},
		{"max_diff_runes_per_file", c.MaxDiffRunesPerFile},
	} {
		if cap.val < 1 {
			return fmt.Errorf("config: %s must be ≥ 1, got %d", cap.name, cap.val)
		}
	}
	if c.Triage.Enabled {
		switch c.Triage.Mode {
		case "", triage.ModeWeek, triage.ModeMR:
		default:
			return fmt.Errorf("config: triage.mode must be %q or %q, got %q", triage.ModeWeek, triage.ModeMR, c.Triage.Mode)
		}
	}
	c.InternalOnlyProjects = trimAll(c.InternalOnlyProjects)
	return nil
}

// trimAll drops surrounding whitespace from a list of operator-supplied
// strings, and drops the entries that were whitespace only.
//
// The --internal-only-project flag already does this (stringSliceFlag
// trims each value), but the YAML block did not — so a list pasted out
// of a spreadsheet behaved differently from the same list typed as a
// flag, and an entry like "  group/secret  " could never match a
// project path. The two sources are documented as unioning together, so
// they have to normalise the same way. A missed match here is an
// audience control failing open: the project gets customer-facing
// output it was marked as never having.
func trimAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Load reads path (if it exists) and unmarshals into a fresh Config. A
// missing file is not an error — it returns a zero Config plus nil. Other
// I/O or parse errors are returned.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// KnownFields rejects a key the struct does not declare. Without it
	// a typo is silent: `max_event` left the cap at its default, and a
	// typo in `publish.enabled` disabled publishing while the run still
	// reported success. examples/summarizer.yaml documents every key,
	// which makes a typo the likely failure rather than an exotic one.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}
