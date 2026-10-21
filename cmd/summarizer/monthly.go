package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/run"
)

// monthlyCmd rolls up one calendar month of already-published weekly
// rollups into a month-end file per audience.
//
// It reads from the GitLab target repo rather than re-running the
// pipeline: September's weekly rollups already exist as text, so the
// job is a single LLM call over them, not a second pass of per-project
// calls. Requires publish.enabled with a GitLab target.
func monthlyCmd(args []string) int {
	fs := flag.NewFlagSet("monthly", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `summarizer monthly — roll up a month's weekly rollups

Reads the weekly combined rollups already published to the GitLab
target repo and writes one month-end file per audience. Weeks with no
published rollup are skipped and listed.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Config: reads the same summarizer.yaml as 'run'. Requires a publish:
block with enabled: true — there is nothing to roll up without it.

Env:
  GITLAB_REPORTS_TOKEN  required; the token that can read the target repo
  OPENAI_API_KEY        required when --provider openai (default)
  ANTHROPIC_API_KEY     required when --provider anthropic

Example:
  summarizer monthly                      # previous calendar month
  summarizer monthly --month 2026-09      # a specific month
`)
	}

	var (
		month         = fs.String("month", "", "month to roll up, YYYY-MM. Default: the previous calendar month.")
		provider      = fs.String("provider", "", "LLM provider: openai or anthropic (default from config)")
		model         = fs.String("model", "", "LLM model id (default from config)")
		dryRun        = fs.Bool("dry-run", false, "print the assembled prompt; no LLM call, no publish")
		configPath    = fs.String("config", "./summarizer.yaml", "path to YAML config file")
		gitlabHost    = fs.String("gitlab-host", "", "GitLab base URL (default from config)")
		openaiKey     = fs.String("openai-key", "", "OpenAI API key (default: $OPENAI_API_KEY). Prefer env var.")
		openaiBaseURL = fs.String("openai-base-url", "", "OpenAI-compatible base URL (default from config/env)")
		anthropicKey  = fs.String("anthropic-key", "", "Anthropic API key (default: $ANTHROPIC_API_KEY). Prefer env var.")
		safetyURL     = fs.String("safety-url", "", "decision API base URL for the prompt-injection guard (default from config safety.decision_url)")
		safetyToken   = fs.String("safety-token", "", "decision API token (default: $SAFETY_DECISION_TOKEN env or config)")
		safetyEnabled = fs.Bool("safety-enabled", false, "gate every LLM call through the prompt-injection detector; hard-fails on verdict. Off by default; config safety.enabled turns it on too.")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: config: %v\n", err)
		return exitConfig
	}
	if *gitlabHost != "" {
		cfg.GitLabHost = *gitlabHost
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *openaiBaseURL != "" {
		cfg.OpenAIBaseURL = *openaiBaseURL
	} else if env := os.Getenv("OPENAI_BASE_URL"); env != "" {
		cfg.OpenAIBaseURL = env
	}
	if *provider != "" {
		cfg.Provider = *provider
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitConfig
	}
	if !cfg.Publish.Enabled {
		fmt.Fprintln(os.Stderr, "error: summarizer monthly needs a publish: block with enabled: true — there are no published weekly rollups to roll up otherwise")
		return exitConfig
	}

	pub, code := newPublisher(cfg)
	if code != exitOK {
		return code
	}
	glPub, ok := pub.(*publisher.GitLabPublisher)
	if !ok {
		fmt.Fprintln(os.Stderr, "error: summarizer monthly needs the GitLab publisher")
		return exitConfig
	}

	llmProvider, llmCode := resolveLLM(cfg, *openaiKey, *anthropicKey)
	if llmCode != exitOK {
		return llmCode
	}

	// The guard is not optional here: `summarizer monthly` feeds last
	// month's published text straight back to the LLM, and that text is
	// itself derived from GitLab issue and MR bodies. Same control as
	// `run` — otherwise `safety.enabled: true` is a partial promise.
	guard, guardCode := newGuard(cfg, *safetyEnabled, *safetyURL, *safetyToken)
	if guardCode != exitOK {
		return guardCode
	}

	runner := &run.MonthlyRunner{
		Publisher:    glPub,
		Reader:       glPub,
		LLM:          llmProvider,
		Guard:        guard,
		Model:        cfg.Model,
		DryRun:       *dryRun,
		ReportPrefix: cfg.Publish.Prefix,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	res, err := runner.Run(ctx, *month)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: monthly rollup: %v\n", err)
		return exitGitLab
	}

	fmt.Fprintf(os.Stderr, "summarizer: monthly %s read %d week(s): %s",
		res.Month, len(res.Weeks), strings.Join(res.Weeks, " "))
	if len(res.Skipped) > 0 {
		fmt.Fprintf(os.Stderr, "; skipped %s", strings.Join(res.Skipped, ", "))
	}
	fmt.Fprintln(os.Stderr)
	for _, f := range res.Files {
		fmt.Fprintln(os.Stderr, f)
	}
	return exitOK
}
