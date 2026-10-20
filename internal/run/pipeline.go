// Package run orchestrates the summarizer pipeline: fetch → normalize →
// prompt ×2 → complete ×2 → write. The pipeline is the only place that
// imports every other internal package concretely; cmd/summarizer wires it.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/policy"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/safety"
)

// Options are run-time parameters resolved at the CLI boundary.
type Options struct {
	Projects             []string      // required (unless DiscoveredGroups is set): one or more GitLab paths "group/subgroup/proj"
	Since                time.Duration // window: now - Since < since
	Provider             string        // "openai" or "anthropic" (overrides config)
	Model                string        // overrides config
	Force                bool          // overwrite existing reports
	DryRun               bool          // fetch + print + exit; no LLM call, no file write
	IncludeDiffs         bool          // --include-diffs: fetch per-MR diff bodies and feed them into the team prompt
	DiscoveredGroups     []string      // --discover-group: each entry is a group path; the pipeline fetches the group's projects, probes each for activity, and includes the active ones in the run. Composes with Projects.
	InternalOnlyProjects []string      // --internal-only-project (repeatable) + YAML internal_only_projects block. Union with topic-based "summarize-team-only" classification; team-only projects produce no customer-facing report.
}

// maxActiveEpics caps the number of epic headers that appear in a
// per-project report. Applies to ACTIVE epics only — see
// events.FilterActiveEpics. A project with 50 dormant epics and 5
// active ones in-window will surface all 5 even though the dormant
// ones never appear. Set conservatively because each active epic's
// nested events add roughly 200–400 tokens to the prompt; 20 keeps
// even a 4-project rollup under 16K tokens for the epic section.
const maxActiveEpics = 20

// ProjectResult describes what one project produced. Used to feed the
// rollup step (when len(Projects) > 1) without re-reading from disk.
type ProjectResult struct {
	Project          string
	Source           policy.ProjectSource // SourceDiscovered for --discover-group; SourceExplicit otherwise
	EventsFetched    int
	EventsUsed       int
	CustomerText     string // rendered markdown for the customer audience (empty when TeamOnly)
	TeamText         string // rendered markdown for the team audience
	FilesWritten     []string
	TokensByAudience map[string]llm.Usage
	FromDiscovery    bool           // alias for Source == SourceDiscovered; retained for log readability
	TeamOnly         bool           // true ⇒ no customer-facing content was produced for this project
	CustomerOn       bool           // true ⇒ customer-facing content was produced (== !TeamOnly)
	ActiveEpics      []*gitlab.Epic // active epics for this project (had issue activity in window); used by runEpicRollup
}

// Result describes what the run produced. Used by main to write the stderr
// run summary. Projects is the per-project list; RollupFiles holds the
// combined-customer.md and combined-team.md paths when len(Projects) > 1.
// EpicRollupFiles holds combined-epics--{audience}.md when ≥1 active epic
// was found across any project.
type Result struct {
	Projects         []ProjectResult
	RollupFiles      []string
	EpicRollupFiles  []string
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

	// lastActiveEpics is populated during runOneProject so the
	// subsequent runEpicRollup step can publish the cross-project
	// epic rollup without re-fetching. Cleared at the start of
	// every Run.
	lastActiveEpics map[string][]*gitlab.Epic
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
//
// Discovery: when --discover-group is supplied, the pipeline first
// resolves each group's projects, probes each for activity (a single
// cheap MR-list call per project), and adds the active ones to the
// run. The explicit --project list and the discovered list compose:
// duplicates collapse to the explicit listing. The cross-project
// rollup covers ALL active projects in the run — explicit + discovered
// together.
func (p *Pipeline) Run(ctx context.Context) (Result, error) {
	res := Result{Projects: []ProjectResult{}, TokensByAudience: map[string]llm.Usage{}}
	p.lastActiveEpics = make(map[string][]*gitlab.Epic)

	if len(p.Opts.Projects) == 0 && len(p.Opts.DiscoveredGroups) == 0 {
		return res, errors.New("at least one --project or --discover-group is required")
	}

	finalList, discoveredSet, err := p.resolveProjects(ctx)
	if err != nil {
		return res, err
	}
	if len(finalList) == 0 {
		// Discovery ran and found nothing active. Surface a
		// stderr line so the operator can tell discovery ran vs
		// crashed silently.
		fmt.Fprintln(os.Stderr, "summarizer: no active projects after resolution (explicit=0, discovered=0); nothing to do")
		return res, nil
	}

	// Per-project loop. Each project produces two files (customer +
	// team); the rollup step (below) adds two more when there's more
	// than one project. Failures on one project don't stop the others —
	// the partial-result contract is the same as the single-project
	// path's.
	for _, project := range finalList {
		pr, err := p.runOneProject(ctx, project)
		// Tag whether this project came from discovery. Consumed by
		// the operator-visible summary so a wide-net run is auditable.
		pr.FromDiscovery = discoveredSet[project]
		// Active epics were already fetched inside runOneProject
		// (via fetchProjectEpics); carry them forward on the
		// ProjectResult so runEpicRollup can publish the cross-
		// project epic rollup without re-fetching.
		pr.ActiveEpics = p.lastActiveEpics[project]
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

	// Cross-project epic rollup — fires whenever ≥1 project has
	// active epics. Independent of project count (a single project
	// with rich epic activity still benefits from a dedicated page).
	// Skipped on --dry-run to keep the no-write contract.
	if !p.Opts.DryRun {
		if len(p.lastActiveEpics) > 0 {
			epicRollupFiles, err := p.runEpicRollup(ctx, res.Projects)
			if err != nil {
				return res, fmt.Errorf("epics rollup: %w", err)
			}
			res.EpicRollupFiles = epicRollupFiles
		}
	}

	return res, nil
}

// runOneProject is the single-project slice of the pipeline: fetch +
// normalize + audience-rendered LLM calls + file writes. Returns a
// ProjectResult even on partial failure (file write fail after a
// successful LLM call still records the LLM call's text).
//
// Audience policy (D5) is decided once at the top of this function:
// the customer iteration is skipped entirely when the project's
// policy says TeamOnly (no LLM call, no file, no Epic ordering
// through the customer audience). The team iteration always runs.
// The classification is computed from source (Discovered vs
// Explicit), the project's GitLab topics, and the three sources of
// team-only override (config / CLI / topic) — see policy.Classify
// for the truth table.
func (p *Pipeline) runOneProject(ctx context.Context, project string) (ProjectResult, error) {
	res := ProjectResult{
		Project:          project,
		Source:           policy.SourceExplicit, // overridden to SourceDiscovered in Run when applicable
		FilesWritten:     []string{},
		TokensByAudience: map[string]llm.Usage{},
	}

	inOverrideList := containsString(p.Opts.InternalOnlyProjects, project)
	topics := p.projectTopics(ctx, project)
	audience := policy.Classify(res.Source, topics, inOverrideList)
	res.TeamOnly = audience.TeamOnly
	res.CustomerOn = audience.CustomerOn

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

	epics, err := p.fetchProjectEpics(ctx, project, since, time.Now().UTC())
	if err != nil {
		// Epic failure is non-fatal: skip the epic section but keep the
		// run. Anything other than the (already-soft-degraded) 403/404
		// from the epics endpoint is a transport problem, so log it and
		// continue. The flat rendering path runs as if epics weren't
		// configured.
		fmt.Fprintf(os.Stderr, "summarizer: WARN epics unavailable for %s — proceeding without epic section: %v\n", project, err)
		epics = nil
	}
	if len(epics) > maxActiveEpics {
		// Soft warning; if the project has more than the cap's worth
		// of active epics, only the first maxActiveEpics surface in the
		// report (caller already filtered to "had activity in window").
		fmt.Fprintf(os.Stderr, "summarizer: %s: %d active epics in window, capping at %d\n",
			project, len(epics), maxActiveEpics)
		epics = epics[:maxActiveEpics]
	}
	events.AttachEpics(all, epics)
	res.EventsFetched = len(all)
	res.EventsUsed = min(len(all), p.Cfg.MaxEvents)

	// Record active epics for runEpicRollup (read by Run, not by the
	// per-project LLM call below). Allocated lazily so projects with
	// no epics don't add map entries.
	if len(epics) > 0 {
		p.lastActiveEpics[project] = epics
	}

	if p.Opts.DryRun {
		fmt.Fprintf(os.Stderr, "summarizer dry-run: %s: %d events (%d mrs, %d issues, %d commits) in window [%s, %s]\n",
			project, len(all), len(mrs), len(issues), len(commits),
			since.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		return res, nil
	}

	window := p.Opts.Since
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		if aud == prompt.AudienceCustomer && !res.CustomerOn {
			// Audience policy (D5) says team-only. Skip the
			// customer iteration entirely: no LLM call, no file
			// write, no event rendering for the customer side.
			// Team iteration runs as usual.
			continue
		}

		system, user, err := prompt.Render(aud, project, window, all, p.Cfg.MaxEvents, epics)
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
		// Audience policy (D5): filter projects by audience. The
		// customer-facing rollup only sees projects with CustomerOn;
		// the team-facing rollup sees all projects. When the
		// customer-facing list is empty (all-team-only run), skip
		// the customer rollup entirely — no file should land
		// containing zero customer-eligible projects.
		var customerEligible []ProjectResult
		if aud == prompt.AudienceCustomer {
			for _, pr := range projects {
				if pr.CustomerOn {
					customerEligible = append(customerEligible, pr)
				}
			}
			if len(customerEligible) == 0 {
				continue
			}
		} else {
			customerEligible = projects
		}

		// Pick the right per-audience text per project. Per-project
		// results carry both texts; we feed the rollup the same
		// audience the per-project report was written for.
		reports := make([]prompt.ProjectReport, 0, len(customerEligible))
		for _, pr := range customerEligible {
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

// runEpicRollup publishes one cross-project epic rollup file per
// audience, paralleling runRollup. Distinct from runRollup in two
// ways:
//   - Trigger: ≥1 active epic anywhere in the run, NOT "≥2
//     projects". A single project with rich epic activity still
//     benefits from a dedicated page that focuses on objective
//     progress.
//   - Surface: the file name carries `epics` (combined--epics--...
//     vs combined--... so a docbuilder operator browsing the
//     period directory can tell the two apart without rendering).
//
// Skipped when no project has active epics (gated in Run, not
// here). The rollup is one LLM call per audience; the prompt is
// trim — only epic-level metadata (title, state, web URL, event
// count), NOT per-event detail, so the model's job is cross-
// project theme synthesis, not narration.
func (p *Pipeline) runEpicRollup(ctx context.Context, projects []ProjectResult) ([]string, error) {
	var files []string
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		// Audience policy (D5): same filtering as runRollup. The
		// customer-facing epic rollup only sees epics from projects
		// with CustomerOn == true; the team-facing rollup sees all
		// epics. Skip the customer audience entirely if no customer-
		// eligible epics exist.
		var eligible []ProjectResult
		if aud == prompt.AudienceCustomer {
			for _, pr := range projects {
				if pr.CustomerOn {
					eligible = append(eligible, pr)
				}
			}
			if len(eligible) == 0 {
				continue
			}
		} else {
			eligible = projects
		}
		entries := make([]prompt.EpicRollupEntry, 0, len(eligible))
		for _, pr := range eligible {
			if len(pr.ActiveEpics) == 0 {
				continue
			}
			summaries := make([]prompt.EpicSummary, 0, len(pr.ActiveEpics))
			for _, e := range pr.ActiveEpics {
				// EventCount is the activity-breadth signal for the
				// rollup. We approximate it from the rendered body
				// of the per-project report — a simpler tally would
				// be "any epic with at least one issue updated in
				// the window" but the event-count gives the model
				// more useful info for prioritizing.
				count := countEpicEvents(pr, e)
				summaries = append(summaries, prompt.EpicSummary{Epic: e, EventCount: count})
			}
			entries = append(entries, prompt.EpicRollupEntry{
				Project: pr.Project,
				Epics:   summaries,
			})
		}
		if len(entries) == 0 {
			continue
		}
		// Pick a representative project name for the report header
		// (the rollup spans N projects; we surface a single project
		// path in the doc-builder frontmatter — first by sorted
		// order for determinism).
		repProject := ""
		if len(projects) > 0 {
			repProject = projects[0].Project
		}
		system, user, err := prompt.RenderEpicRollup(aud, repProject, p.Opts.Since, entries)
		if err != nil {
			return files, fmt.Errorf("render epics rollup %s: %w", aud, err)
		}
		if err := p.checkPrompt(ctx, "epics-rollup", aud, system+"\n\n"+user); err != nil {
			return files, err
		}
		resp, err := p.LLM.Complete(ctx, llm.Request{
			System:    system,
			User:      user,
			Model:     p.Opts.Model,
			MaxTokens: 2048,
		})
		if err != nil {
			return files, fmt.Errorf("llm epics rollup %s: %w", aud, err)
		}

		name := epicRollupFilename(p.Opts.Since, aud)
		period := periodSlug(p.Opts.Since)
		res2, err := p.Publisher.Publish(publisher.Request{
			Name:    name,
			Body:    []byte(resp.Text),
			Force:   p.Opts.Force,
			Kind:    publisher.KindRollup,
			Period:  period,
			Title:   "epics rollup " + string(aud),
			Project: "",
			Public:  aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return files, fmt.Errorf("write epics rollup %s: %w", aud, err)
		}
		files = append(files, res2.Ref)
	}
	return files, nil
}

// countEpicEvents tallies how many lines in the per-project report's
// issue-section mention this epic. It's an approximation — the
// rendered body doesn't carry Epic references per-line — but it
// gives the cross-project rollup a sense of activity breadth
// without re-fetching any data. For the customer audience this
// number is the model's only signal beyond the epic's own title;
// for the team audience it's a coarse health check.
//
// Implementation walks the body once per epic. Project reports
// are short (cap-events at 200, default), so this is cheap.
func countEpicEvents(pr ProjectResult, e *gitlab.Epic) int {
	body := pr.CustomerText + pr.TeamText
	count := 0
	// We count occurrences of the epic's title as a rough proxy
	// for "events tagged with this epic". Real counting would
	// require carrying per-event Epic refs into ProjectResult;
	// that data is available in runOneProject's all slice but
	// would inflate the struct. This heuristic is good enough
	// for the cross-project rollup's breadth signal.
	needle := e.Title
	if needle == "" {
		return 0
	}
	count += strings.Count(body, needle)
	return count
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
//     can emit a clean stderr line and exit non-zero.
//     This is the case where we DO trust the guard's
//     answer — a deliberate prompt-injection attempt
//     should stop the run, not slide through.
//   - guard error       → soft-fail. A flaky /v1/systemone instance
//     (network blip, transient 5xx, malformed
//     response) does NOT block summary generation.
//     The structural defense — <data> markers, system
//     prompt refusing to obey data-tagged instructions
//     — is still in place; we lose only the second-
//     opinions verdict. Logged as a warning so the
//     operator notices on the next cron run.
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

// fetchProjectEpics fetches the active-epic list for one project:
// resolve the group's full path via the project metadata, list the
// group's epics, fetch each epic's linked issues, then filter to the
// subset that had issue activity in the window. The filter step applies
// the operator's "epics without activity don't count against the cap"
// rule — a project with 50 dormant epics produces zero work; a project
// with 5 active epics produces only the 5 active ones.
//
// Graceful-degrade contract: any (403, 404) from /epics or /epics/:iid/issues
// returns (nil, nil) so the pipeline can skip the section without
// surfacing an error. 401s and 5xxs DO hard-fail the project — those
// are real problems the operator must see.
// nolint:unused // wired via runOneProject
func (p *Pipeline) fetchProjectEpics(ctx context.Context, project string, since, until time.Time) ([]*gitlab.Epic, error) {
	ns, err := p.GitLab.FetchProjectNamespace(ctx, project)
	if err != nil {
		return nil, err
	}
	if ns.Kind != "group" {
		// User-namespace projects don't have a group and therefore
		// don't have epics. Skip silently; not an error.
		return nil, nil
	}
	raw, err := p.GitLab.FetchEpics(ctx, ns.FullPath)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	// Fetch each epic's linked issues. nil-on-soft-degrade per epic:
	// a single epic returning 403 doesn't kill the whole list; we
	// just lose that one's issue map (its events would land in
	// "Other activity").
	for i := range raw {
		rawIssues, err := p.GitLab.FetchEpicIssues(ctx, ns.FullPath, raw[i].IID)
		if err != nil {
			return nil, err
		}
		raw[i].Issues = rawIssues
	}
	all := make([]*gitlab.Epic, 0, len(raw))
	for i := range raw {
		all = append(all, &raw[i])
	}
	return events.FilterActiveEpics(all, since, until), nil
}

// projectTopics fetches the project's GitLab topic list, used by
// D5's audience classifier (see policy.Classify). One extra
// /projects/:id fetch per project per run; the result feeds a
// pure-function classifier so we don't carry the topics around
// past Classify.
//
// On any error (transport, parse, missing field on older GitLab),
// returns nil and lets the classifier default to "no topics" — the
// same behavior as a project whose operator never set topics. The
// err-on-everything alternative would surface a network blip as a
// customer-output suppression failure; not useful.
func (p *Pipeline) projectTopics(ctx context.Context, project string) []string {
	summary, err := p.GitLab.FetchProjectSummary(ctx, project)
	if err != nil || summary == nil {
		return nil
	}
	return summary.Topics
}

// resolveProjects merges explicit (--project) and discovered
// (--discover-group) inputs into the final per-project list, dedupes
// on path (explicit wins on overlap), probes each discovered project
// for activity, and emits a structured manifest to stderr.
//
// Two return values:
//   - finalList: every project to run, in deterministic order
//     (explicit first, then discovered)
//   - discoveredSet: paths that came from discovery. The orchestrator
//     tags each ProjectResult with this so summaries can tell the
//     operator which projects were auto-discovered.
func (p *Pipeline) resolveProjects(ctx context.Context) ([]string, map[string]bool, error) {
	return resolveProjectsWithClient(ctx, p.GitLab, p.Opts)
}

// resolveProjectsWithClient is the testable form: takes a Client
// directly so unit tests don't need a full Pipeline.
func resolveProjectsWithClient(ctx context.Context, gc *gitlab.Client, opts Options) ([]string, map[string]bool, error) {
	finalList := make([]string, 0, len(opts.Projects))
	discoveredSet := make(map[string]bool)

	// Explicit projects always pass through.
	finalList = append(finalList, opts.Projects...)

	if len(opts.DiscoveredGroups) == 0 {
		return finalList, discoveredSet, nil
	}

	since := time.Now().UTC().Add(-opts.Since)
	excluded := 0
	included := 0

	for _, group := range opts.DiscoveredGroups {
		// v1: recursive=false. The recursive branch is reserved
		// for a future change; see FetchGroupProjects.
		projects, err := gc.FetchGroupProjects(ctx, group, false)
		if err != nil {
			return nil, nil, fmt.Errorf("discover group %s: %w", group, err)
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery from group=%s recursive=false; scanned=%d\n",
			group, len(projects))
		for _, proj := range projects {
			// Skip projects already explicit-listed (dedupe wins to
			// the explicit listing: FromDiscovery stays false).
			if containsString(opts.Projects, proj.Path) {
				continue
			}
			active, err := gc.ProbeProjectActivity(ctx, proj.Path, since)
			if err != nil {
				// 4xx (other than 404 already softened) and 5xx fail
				// the whole run via the wrapped error. We could
				// soften further, but a probe-failure signals a real
				// transport / auth issue that's worth surfacing.
				return nil, nil, fmt.Errorf("probe %s: %w", proj.Path, err)
			}
			// Audience classification (D5): discovered projects
			// need either the `summarizer` opt-in topic to be
			// customer-eligible, or they default to team-only (the
			// default-safe behaviour). Config/CLI overrides apply
			// here too. Annotate the manifest line so the operator
			// can audit classifications at a glance.
			inOverride := containsString(opts.InternalOnlyProjects, proj.Path)
			aud := policy.Classify(policy.SourceDiscovered, proj.Topics, inOverride)
			tnoTag := ""
			if aud.TeamOnly {
				tnoTag = " (team-only)"
			}
			if !active {
				excluded++
				fmt.Fprintf(os.Stderr, "  excluded (no MRs in window): %s%s last_activity_at=%s\n",
					proj.Path, tnoTag, proj.LastActivityAt.Format(time.RFC3339))
				continue
			}
			included++
			finalList = append(finalList, proj.Path)
			discoveredSet[proj.Path] = true
			fmt.Fprintf(os.Stderr, "  included:%s %s last_activity_at=%s\n",
				tnoTag, proj.Path, proj.LastActivityAt.Format(time.RFC3339))
		}
	}
	fmt.Fprintf(os.Stderr, "summarizer: discovery summary included=%d excluded=%d\n", included, excluded)
	return finalList, discoveredSet, nil
}

// resolveProjectsForTest wraps resolveProjectsWithClient for use by
// tests that don't construct a full Pipeline. Returns the same shape
// (final list + discovered-set) without the legacy stderr-lines arg
// the older test fixtures assumed.
func resolveProjectsForTest(gc *gitlab.Client, opts Options, _ time.Time) (finalList []string, discoveredSet map[string]bool, err error) {
	return resolveProjectsWithClient(context.Background(), gc, opts)
}

// containsString is a small helper used to skip already-explicit
// projects during the discovery loop. Hand-rolled linear scan: the
// project list is small and slices.Contains would add an import.
func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
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

// epicRollupFilename produces the cross-project epic rollup file
// name. Distinct prefix (`epics`) so a docbuilder operator browsing
// the period directory can tell it from the per-project rollup at
// a glance. Two of these per run (customer + team) when ≥1 active
// epic exists anywhere in the run.
func epicRollupFilename(window time.Duration, aud prompt.Audience) string {
	days := int(window / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("combined--epics--%dd--%s.md", days, aud)
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
