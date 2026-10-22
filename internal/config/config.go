// Package config loads summarizer configuration from a YAML file, with env
// vars and CLI flags resolved at the boundary in cmd/summarizer. The point
// of this package is one struct, one Load() function, and one Validate().
//
// Resolution precedence (highest wins):
//  1. CLI flag (applied by main before Load is called)
//  2. Env var
//  3. YAML file
//  4. Built-in default
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
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
	return nil
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
