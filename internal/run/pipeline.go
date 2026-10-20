// Package run orchestrates the summarizer pipeline: fetch → normalize →
// prompt ×2 → complete ×2 → write. The pipeline is the only place that
// imports every other internal package concretely; cmd/summarizer wires it.
package run

import (
	"context"
	"errors"
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
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/safety"
)

// Options are run-time parameters resolved at the CLI boundary.
type Options struct {
	Projects    []string      // required: one or more GitLab paths "group/subgroup/proj"
	Since       time.Duration // window: now - Since < since
	Provider    string        // "openai" or "anthropic" (overrides config)
	Model       string        // overrides config
	Force       bool          // overwrite existing reports
	DryRun      bool          // fetch + print + exit; no LLM call, no file write
	IncludeDiffs bool         // --include-diffs: fetch per-MR diff bodies and feed them into the team prompt
}

// ProjectResult describes what one project produced. Used to feed the
// rollup step (when len(Projects) > 1) without re-reading from disk.
type ProjectResult struct {
	Project         string
	EventsFetched   int
	EventsUsed      int
	CustomerText    string // rendered markdown for the customer audience
	TeamText        string // rendered markdown for the team audience
	FilesWritten    []string
	TokensByAudience map[string]llm.Usage
}

// Result describes what the run produced. Used by main to write the stderr
// run summary. Projects is the per-project list; RollupFiles holds the
// combined-customer.md and combined-team.md paths when len(Projects) > 1.
type Result struct {
	Projects      []ProjectResult
	RollupFiles   []string
	TokensByAudience map[string]llm.Usage // rollup token usage, when applicable
}

// Pipeline is the configured orchestrator. Construct in main.
type Pipeline struct {
	Cfg       *config.Config
	GitLab    *gitlab.Client
	LLM       llm.Provider
	Publisher publisher.Publisher
	Guard     safety.Guard // nil-safe; NoopGuard is substituted when safety is disabled
	Opts      Options
}

// ErrInjection is returned (wrapped) when the safety guard classifies
// the rendered prompt as a prompt-injection attempt and the pipeline
// policy is hard-fail.
var ErrInjection = errors.New("safety guard classified prompt as injection")

// New wires the orchestrator. Caller passes in already-constructed
// clients. publisher is chosen by main based on config (local disk
// by default, GitLab MR when PublishConfig.Enabled is true). Pass
// safety.NoopGuard{} to disable the check.
func New(cfg *config.Config, gc *gitlab.Client, p llm.Provider, pub publisher.Publisher, g safety.Guard, opts Options) *Pipeline {
	if g == nil {
		g = safety.NoopGuard{}
	}
	return &Pipeline{Cfg: cfg, GitLab: gc, LLM: p, Publisher: pub, Guard: g, Opts: opts}
}

// Run executes the pipeline. Returns a Result on success; on failure
// returns a descriptive error and the partial Result so main can still
// emit a run summary on stderr before exiting non-zero.
func (p *Pipeline) Run(ctx context.Context) (Result, error) {
	res := Result{Projects: []ProjectResult{}, TokensByAudience: map[string]llm.Usage{}}

	if len(p.Opts.Projects) == 0 {
		return res, errors.New("at least one --project is required")
	}

	// Per-project loop. Each project produces two files (customer +
	// team); the rollup step (below) adds two more when there's more
	// than one project. Failures on one project don't stop the others —
	// the partial-result contract is the same as the single-project
	// path's.
	for _, project := range p.Opts.Projects {
		pr, err := p.runOneProject(ctx, project)
		res.Projects = append(res.Projects, pr)
		if err != nil {
			return res, fmt.Errorf("project %s: %w", project, err)
		}
	}

	if len(res.Projects) > 1 && !p.Opts.DryRun {
		rollupFiles, err := p.runRollup(ctx, res.Projects)
		if err != nil {
			return res, fmt.Errorf("rollup: %w", err)
		}
		res.RollupFiles = rollupFiles
	}

	return res, nil
}

// runOneProject is the single-project slice of the pipeline: fetch +
// normalize + 2 LLM calls + 2 file writes. Returns a ProjectResult
// even on partial failure (file write fail after a successful LLM call
// still records the LLM call's text).
func (p *Pipeline) runOneProject(ctx context.Context, project string) (ProjectResult, error) {
	res := ProjectResult{
		Project:         project,
		FilesWritten:    []string{},
		TokensByAudience: map[string]llm.Usage{},
	}

	since := time.Now().UTC().Add(-p.Opts.Since)

	mrs, err := p.GitLab.FetchMRs(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch mrs: %w", err)
	}
	fillMRChanges(ctx, p.GitLab, project, mrs)
	if p.Opts.IncludeDiffs {
		fillMRDetails(ctx, p.GitLab, project, mrs)
	}

	issues, err := p.GitLab.FetchIssues(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch issues: %w", err)
	}
	commits, err := p.GitLab.FetchCommits(ctx, project, since)
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
		fmt.Fprintf(os.Stderr, "summarizer dry-run: %s: %d events (%d mrs, %d issues, %d commits) in window [%s, %s]\n",
			project, len(all), len(mrs), len(issues), len(commits),
			since.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		return res, nil
	}

	window := p.Opts.Since
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		system, user, err := prompt.Render(aud, project, window, all, p.Cfg.MaxEvents)
		if err != nil {
			return res, fmt.Errorf("render %s: %w", aud, err)
		}
		if err := p.checkPrompt(ctx, project, aud, system+"\n\n"+user); err != nil {
			return res, err
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

		var text string
		switch aud {
		case prompt.AudienceCustomer:
			text = resp.Text
			res.CustomerText = resp.Text
		case prompt.AudienceTeam:
			text = resp.Text
			res.TeamText = resp.Text
		}

		name := filename(window, project, aud)
		period := periodSlug(p.Opts.Since)
		res2, err := p.Publisher.Publish(publisher.Request{
			Name:    name,
			Body:    []byte(text),
			Force:   p.Opts.Force,
			Kind:    publisher.KindPerProject,
			Period:  period,
			Title:   string(aud),
			Project: project,
			Public:  aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return res, fmt.Errorf("write %s: %w", aud, err)
		}
		res.FilesWritten = append(res.FilesWritten, res2.Ref)
	}

	return res, nil
}

// runRollup runs one extra LLM call per audience over the per-project
// reports and writes the combined files. Only called when len(Projects)
// > 1. The rollup prompt takes per-project reports as input and asks the
// model to surface cross-project themes.
func (p *Pipeline) runRollup(ctx context.Context, projects []ProjectResult) ([]string, error) {
	var files []string
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		// Pick the right per-audience text per project. Per-project
		// results carry both texts; we feed the rollup the same audience
		// the per-project report was written for.
		reports := make([]prompt.ProjectReport, 0, len(projects))
		for _, pr := range projects {
			body := pr.TeamText
			if aud == prompt.AudienceCustomer {
				body = pr.CustomerText
			}
			reports = append(reports, prompt.ProjectReport{Name: pr.Project, Body: body})
		}
		system, user, err := prompt.RenderRollup(aud, p.Opts.Since, reports)
		if err != nil {
			return files, fmt.Errorf("render rollup %s: %w", aud, err)
		}
		if err := p.checkPrompt(ctx, "rollup", aud, system+"\n\n"+user); err != nil {
			return files, err
		}
		resp, err := p.LLM.Complete(ctx, llm.Request{
			System:    system,
			User:      user,
			Model:     p.Opts.Model,
			MaxTokens: 2048,
		})
		if err != nil {
			return files, fmt.Errorf("llm rollup %s: %w", aud, err)
		}

		name := rollupFilename(p.Opts.Since, aud)
		period := periodSlug(p.Opts.Since)
		res2, err := p.Publisher.Publish(publisher.Request{
			Name:    name,
			Body:    []byte(resp.Text),
			Force:   p.Opts.Force,
			Kind:    publisher.KindRollup,
			Period:  period,
			Title:   "rollup " + string(aud),
			Project: "",
			Public:  aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return files, fmt.Errorf("write rollup %s: %w", aud, err)
		}
		files = append(files, res2.Ref)
	}
	return files, nil
}

// periodSlug returns the YYYY-MM-DD slug for the period end-date. The
// GitLab publisher uses this as the source-branch suffix so a re-run on
// the same day lands on the same branch (and updates the same MR) rather
// than spawning a new branch per re-invocation.
func periodSlug(window time.Duration) string {
	return time.Now().UTC().Format("2006-01-02")
}

// checkPrompt runs the rendered prompt through the safety guard.
//
// Verdict handling:
//   - "safe"            → continue with the LLM call (logged at info level)
//   - "injection"       → hard-fail. ErrInjection wraps the score so main
//                         can emit a clean stderr line and exit non-zero.
//                         This is the case where we DO trust the guard's
//                         answer — a deliberate prompt-injection attempt
//                         should stop the run, not slide through.
//   - guard error       → soft-fail. A flaky /v1/systemone instance
//                         (network blip, transient 5xx, malformed
//                         response) does NOT block summary generation.
//                         The structural defense — <data> markers, system
//                         prompt refusing to obey data-tagged instructions
//                         — is still in place; we lose only the second-
//                         opinions verdict. Logged as a warning so the
//                         operator notices on the next cron run.
func (p *Pipeline) checkPrompt(ctx context.Context, project string, aud prompt.Audience, prompt string) error {
	verdict, score, err := p.Guard.Check(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "summarizer: WARN safety guard unavailable for %s/%s — proceeding without check: %v\n", project, aud, err)
		return nil
	}
	if verdict == safety.VerdictInjection {
		fmt.Fprintf(os.Stderr, "summarizer: safety guard REJECTED prompt for %s/%s (score=%.3f >= threshold)\n", project, aud, score)
		return fmt.Errorf("%w (score=%.3f): %s/%s", ErrInjection, score, project, aud)
	}
	fmt.Fprintf(os.Stderr, "summarizer: safety guard cleared %s/%s (score=%.3f)\n", project, aud, score)
	return nil
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

// fillMRDetails fans out the per-MR /changes call *with diff bodies* across
// changeConcurrency workers and attaches the per-file chunks onto each
// MergeRequest. Same worker pool as fillMRChanges; sequential on the
// per-MR scale so the two can run concurrently if the operator enables
// both (currently they always do — fillMRDetails is only called when
// --include-diffs is on, alongside fillMRChanges).
//
// Failures on a single MR are logged and the MR proceeds with no diff
// data. The prompts that include these chunks use <data> markers and
// instruct the model to "infer what the MR does from the diff —
// paraphrase, never paste code into the report", which treats the diff
// as data, not instructions.
func fillMRDetails(ctx context.Context, gc *gitlab.Client, project string, mrs []gitlab.MergeRequest) {
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
			chunks, err := gc.FetchMRDetail(ctx, project, mrs[i].IID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "summarizer: diff fetch for MR %d failed: %v (skipping)\n", mrs[i].IID, err)
				return
			}
			mrs[i].DiffChunks = chunks
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

// rollupFilename is the combined-report counterpart of filename: a fixed
// "combined" prefix instead of a project name. Two of these per run
// (customer + team) when len(Projects) > 1.
func rollupFilename(window time.Duration, aud prompt.Audience) string {
	days := int(window / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("combined--%dd--%s.md", days, aud)
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