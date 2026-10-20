// Package config loads summarizer configuration from a YAML file, with env
// vars and CLI flags resolved at the boundary in cmd/summarizer. The point
// of this package is one struct, one Load() function, and one Validate().
//
// Resolution precedence (highest wins):
//   1. CLI flag (applied by main before Load is called)
//   2. Env var
//   3. YAML file
//   4. Built-in default
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the resolved summarizer configuration for one run.
type Config struct {
	// Required: GitLab host (e.g. "https://gitlab.example.com"). Has no
	// default — refusing to start without it is the correct behavior.
	GitLabHost string

	// Per-project default model for the chosen provider. Default: gpt-4o-mini.
	// Provider-specific: an Anthropic model id if provider="anthropic".
	Model string `yaml:"model"`

	// Output directory for the two reports. Default: ./summarizer-out.
	OutputDir string `yaml:"output_dir"`

	// Default lookback window when --since is not provided on the CLI.
	// CLI flag overrides this. Format is a Go duration: "168h", "720h".
	DefaultLookback string `yaml:"default_lookback"`

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

	// Projects is the list of GitLab project paths to summarize in one run.
	// CLI --project is repeatable; each occurrence adds to the slice. The
	// YAML key accepts a list, e.g.:
	//   projects:
	//     - group/subgroup/project-a
	//     - group/subgroup/project-b
	// When len(Projects) > 1, the pipeline also writes a top-level
	// rollup (combined--<since>d--customer.md and --team.md) by feeding
	// the per-project reports to one extra LLM call per audience.
	Projects []string `yaml:"projects"`

	// Publish configures the optional GitLab MR-based publisher.
	// When enabled, the local disk writer is skipped and every report
	// file is committed to a per-period branch with a reviewable MR
	// against the configured default branch.
	Publish PublishConfig `yaml:"publish"`
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

// Defaults fills in zero values with sensible built-ins.
func (c *Config) Defaults() {
	if c.Model == "" {
		c.Model = "gpt-4o-mini"
	}
	if c.OutputDir == "" {
		c.OutputDir = "./summarizer-out"
	}
	if c.DefaultLookback == "" {
		c.DefaultLookback = "168h" // 7 days
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
	if _, err := time.ParseDuration(c.DefaultLookback); err != nil {
		return fmt.Errorf("config: default_lookback %q is not a Go duration: %w", c.DefaultLookback, err)
	}
	if c.MaxEvents < 1 {
		return fmt.Errorf("config: max_events must be ≥ 1, got %d", c.MaxEvents)
	}
	return nil
}

// Lookback returns DefaultLookback parsed as a Go duration.
func (c *Config) Lookback() time.Duration {
	d, _ := time.ParseDuration(c.DefaultLookback)
	return d
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
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}