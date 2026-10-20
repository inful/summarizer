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