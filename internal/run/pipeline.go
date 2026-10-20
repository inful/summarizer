// Package run orchestrates the summarizer pipeline: fetch → normalize →
// prompt ×2 → complete ×2 → write. The pipeline is the only place that
// imports every other internal package concretely; cmd/summarizer wires it.
package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/writer"
)

// Options are run-time parameters resolved at the CLI boundary.
type Options struct {
	Project  string        // required, GitLab path "group/subgroup/proj"
	Since    time.Duration // window: now - Since < since
	Provider string        // "openai" or "anthropic" (overrides config)
	Model    string        // overrides config
	Force    bool          // overwrite existing reports
	DryRun   bool          // fetch + print + exit; no LLM call, no file write
}

// Result describes what the run produced. Used by main to write the stderr
// run summary.
type Result struct {
	EventsFetched   int
	EventsUsed      int
	FilesWritten    []string
	TokensByAudience map[string]llm.Usage
}

// Pipeline is the configured orchestrator. Construct in main.
type Pipeline struct {
	Cfg     *config.Config
	GitLab  *gitlab.Client
	LLM     llm.Provider
	Writer  *writer.Writer
	Opts    Options
}

// New wires the orchestrator. Caller passes in already-constructed clients.
func New(cfg *config.Config, gc *gitlab.Client, p llm.Provider, w *writer.Writer, opts Options) *Pipeline {
	return &Pipeline{Cfg: cfg, GitLab: gc, LLM: p, Writer: w, Opts: opts}
}

// Run executes the pipeline. Returns a Result on success; on failure
// returns a descriptive error and the partial Result so main can still
// emit a run summary on stderr before exiting non-zero.
func (p *Pipeline) Run(ctx context.Context) (Result, error) {
	res := Result{FilesWritten: []string{}, TokensByAudience: map[string]llm.Usage{}}

	since := time.Now().UTC().Add(-p.Opts.Since)

	mrs, err := p.GitLab.FetchMRs(ctx, p.Opts.Project, since)
	if err != nil {
		return res, fmt.Errorf("fetch mrs: %w", err)
	}
	// Per-MR /changes calls. The list endpoint doesn't expose per-file
	// stats; we need one extra round-trip per MR to surface "this MR was
	// 5 lines vs 30 files". Fan out to changeConcurrency workers so a
	// busy week doesn't serialize the wall clock.
	fillMRChanges(ctx, p.GitLab, p.Opts.Project, mrs)

	issues, err := p.GitLab.FetchIssues(ctx, p.Opts.Project, since)
	if err != nil {
		return res, fmt.Errorf("fetch issues: %w", err)
	}
	commits, err := p.GitLab.FetchCommits(ctx, p.Opts.Project, since)
	if err != nil {
		return res, fmt.Errorf("fetch commits: %w", err)
	}

	all := make([]events.NormalizedEvent, 0, len(mrs)+len(issues)+len(commits))
	for _, m := range mrs {
		all = append(all, events.MR(m))
	}
	for _, i := range issues {
		all = append(all, events.Issue(i))
	}
	for _, c := range commits {
		all = append(all, events.Commit(c))
	}
	res.EventsFetched = len(all)
	res.EventsUsed = min(len(all), p.Cfg.MaxEvents)

	if p.Opts.DryRun {
		fmt.Fprintf(os.Stderr, "summarizer dry-run: %d events (%d mrs, %d issues, %d commits) in window [%s, %s]\n",
			len(all), len(mrs), len(issues), len(commits),
			since.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		return res, nil
	}

	window := p.Opts.Since
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		system, user, err := prompt.Render(aud, p.Opts.Project, window, all, p.Cfg.MaxEvents)
		if err != nil {
			return res, fmt.Errorf("render %s: %w", aud, err)
		}
		resp, err := p.LLM.Complete(ctx, llm.Request{
			System:    system,
			User:      user,
			Model:     p.Opts.Model,
			MaxTokens: 2048,
		})
		if err != nil {
			return res, fmt.Errorf("llm %s: %w", aud, err)
		}
		res.TokensByAudience[string(aud)] = resp.Usage

		name := filename(window, p.Opts.Project, aud)
		path, err := p.Writer.Write(p.Cfg.OutputDir, name, []byte(resp.Text), p.Opts.Force)
		if err != nil {
			return res, fmt.Errorf("write %s: %w", aud, err)
		}
		res.FilesWritten = append(res.FilesWritten, path)
	}

	return res, nil
}

// filename: <sanitized project>--<since-days>d--<audience>.md. The "since"
// is encoded in the filename so re-runs land on different files; combined
// with the refuse-first policy in writer, double-runs are safe.

// changeConcurrency caps how many /merge_requests/:iid/changes calls run
// in parallel. The endpoint is one HTTP round-trip per MR; without this,
// a 50-MR week serializes to ~50 seconds on a slow connection. 4 is small
// enough to stay well under GitLab's per-second rate-limit thresholds on
// most plans, big enough to keep total wall time bounded.
const changeConcurrency = 4

// fillMRChanges fans out the per-MR /changes call across changeConcurrency
// workers and attaches the result onto each MergeRequest. A failure on one
// MR is logged to stderr and the MR proceeds without changes data; we
// don't fail the whole run for one missing per-file summary.
func fillMRChanges(ctx context.Context, gc *gitlab.Client, project string, mrs []gitlab.MergeRequest) {
	if len(mrs) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, changeConcurrency)
	for i := range mrs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := gc.FetchMRChanges(ctx, project, mrs[i].IID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "summarizer: changes fetch for MR %d failed: %v (skipping)\n", mrs[i].IID, err)
				return
			}
			mrs[i].Changes = &s
		}(i)
	}
	wg.Wait()
}

func filename(window time.Duration, project string, aud prompt.Audience) string {
	days := int(window / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("%s--%dd--%s.md", sanitize(project), days, aud)
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, byte(r))
		case r == '/':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "report"
	}
	return string(out)
}

// dirExists is a small helper for the run summary in main; lives here
// because it derives from pipeline state.
func (p *Pipeline) dir() string {
	if abs, err := filepath.Abs(p.Cfg.OutputDir); err == nil {
		return abs
	}
	return p.Cfg.OutputDir
}