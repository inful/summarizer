// Package run orchestrates the summarizer pipeline: fetch → normalize →
// prompt ×2 → complete ×2 → write. The pipeline is the only place that
// imports every other internal package concretely; cmd/summarizer wires it.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
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
	DiscoverByTopic      bool          // --discover-by-topic: scan every project on the instance carrying a `summarize*` topic, across all groups the token can see. Composes with Projects and DiscoveredGroups.
	DiscoverActive       bool          // --discover-active: include every project the token can see that had activity since the window opened. Set automatically when no other discovery input is given, which is the documented default: an active project runs unless it opts out.
	MaxProjects          int           // --max-projects: hard cap on projects fed to the LLM, applied after discovery and opt-out but before any LLM call. Each project costs two completions, so a quarter where 200 repos were touched is ~400 calls; this bounds the bill. 0 = DefaultMaxProjects.
}

// maxActiveEpics caps the number of epic headers that appear in a
// per-project report. Applies to ACTIVE epics only — see
// events.FilterActiveEpics. A project with 50 dormant epics and 5
// active ones in-window will surface all 5 even though the dormant
// ones never appear. Set conservatively because each active epic's
// nested events add roughly 200–400 tokens to the prompt; 20 keeps
// even a 4-project rollup under 16K tokens for the epic section.
const maxActiveEpics = 20

// DefaultMaxProjects caps how many projects one run feeds to the LLM
// when --max-projects is not given. 50 projects is ~100 completions
// before rollups — enough for a busy quarter, bounded enough that an
// instance-wide activity scan cannot produce an unbounded bill.
const DefaultMaxProjects = 50

// ProjectResult describes what one project produced. Used to feed the
// rollup step (when len(Projects) > 1) without re-reading from disk.
type ProjectResult struct {
	Project          string
	EventsFetched    int
	EventsUsed       int
	CustomerText     string // rendered markdown for the customer audience (empty when TeamOnly)
	TeamText         string // rendered markdown for the team audience
	FilesWritten     []string
	TokensByAudience map[string]llm.Usage
	FromDiscovery    bool           // surfaced by --discover-group / --discover-by-topic
	TeamOnly         bool           // true ⇒ no customer-facing content was produced for this project
	CustomerOn       bool           // true ⇒ customer-facing content was produced (== !TeamOnly)
	ActiveEpics      []*gitlab.Epic // active epics for this project (had issue activity in window); used by runEpicRollup
	// AnnotatedEvents is the project's full event set after
	// AttachEpics ran. Kept so the epic rollup can count each epic's
	// events exactly instead of re-deriving them from rendered text.
	AnnotatedEvents []events.NormalizedEvent
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

	if len(p.Opts.Projects) == 0 && len(p.Opts.DiscoveredGroups) == 0 && !p.Opts.DiscoverByTopic && !p.Opts.DiscoverActive {
		return res, errors.New("at least one --project, --discover-group, --discover-by-topic or --discover-active is required")
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

	// Bound the bill before the first LLM call. Each project costs two
	// completions, so the activity-default discovery can produce an
	// unbounded bill on an instance where everything was touched. The
	// cap lands after discovery and opt-out (so it does not count
	// projects that would never have run) and before any LLM call.
	//
	// Sort by path first. finalList is explicit --project entries in
	// argument order, then discovered projects in API response order, so
	// truncating it as-is keeps a fixed cohort that never varies —
	// permanently summarising the same N projects and never the rest.
	// Sorting makes the kept set reproducible and reviewable. It does
	// NOT rotate; that is a policy decision, not a default.
	if cap := p.Opts.maxProjects(); len(finalList) > cap {
		sort.Strings(finalList)
		dropped := finalList[cap:]
		finalList = finalList[:cap]
		fmt.Fprintf(os.Stderr, "summarizer: --max-projects %d — dropping %d project(s) that would have cost %d more LLM call(s):\n",
			cap, len(dropped), 2*len(dropped))
		for _, p := range dropped {
			fmt.Fprintf(os.Stderr, "  dropped: %s\n", p)
		}
	}

	// Per-project loop. Each project produces two files (customer +
	// team); the rollup step (below) adds two more when there's more
	// than one project.
	//
	// A failure on one project does not abort the others. A rate-limited
	// LLM call on project 3 of 25 must not leave projects 4-25
	// unrun — the projects already published stay published either
	// way, so continuing strictly increases what the run delivers. The
	// first error is kept and returned after the loop; the caller
	// still gets a non-zero exit code and a message naming the project.
	var firstErr error
	for _, project := range finalList {
		pr, err := p.runOneProject(ctx, project)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("project %s: %w", project, err)
			}
			fmt.Fprintf(os.Stderr, "summarizer: WARN %s failed, continuing: %v\n", project, err)
			continue
		}
		// Tag whether this project came from discovery. Consumed by
		// the operator-visible summary so a wide-net run is auditable.
		pr.FromDiscovery = discoveredSet[project]
		// Active epics were already fetched inside runOneProject
		// (via fetchProjectEpics); carry them forward on the
		// ProjectResult so runEpicRollup can publish the cross-
		// project epic rollup without re-fetching.
		pr.ActiveEpics = p.lastActiveEpics[project]
		res.Projects = append(res.Projects, pr)
	}
	if firstErr != nil {
		// Return what succeeded alongside the error. The rollups are
		// skipped: a cross-project rollup over a partial set would
		// silently read as a complete week.
		return res, firstErr
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
// The classification is computed from the project's GitLab topics
// and the team-only override list (config / CLI) — see policy.Classify
// for the truth table. How the project entered the run is not an
// input: the audience default is the same either way.
func (p *Pipeline) runOneProject(ctx context.Context, project string) (ProjectResult, error) {
	res := ProjectResult{
		Project:          project,
		FilesWritten:     []string{},
		TokensByAudience: map[string]llm.Usage{},
	}

	inOverrideList := containsString(p.Opts.InternalOnlyProjects, project)
	topics := p.projectTopics(ctx, project)
	audience := policy.Classify(topics, inOverrideList)
	res.TeamOnly = audience.TeamOnly
	res.CustomerOn = audience.CustomerOn

	since := time.Now().UTC().Add(-p.Opts.Since)

	mrs, err := p.GitLab.FetchMRs(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch mrs: %w", err)
	}
	// One /changes call per MR serves both needs. With --include-diffs
	// the same response carries the diff bodies the team prompt wants;
	// without it only the aggregate is used, but there is still no
	// reason to make the call twice.
	fillMRDetail(ctx, p.GitLab, project, mrs, p.Opts.IncludeDiffs)

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

	// Dry-run exits HERE, before the epic fetch. Epics are the most
	// expensive thing this function does — one HTTP call per epic, per
	// project — and a dry run throws the result away. Making the preview
	// slower than the run it previews is how you stop people previewing.
	if p.Opts.DryRun {
		res.EventsFetched = len(all)
		res.EventsUsed = min(len(all), p.Cfg.MaxEvents)
		fmt.Fprintf(os.Stderr, "summarizer dry-run: %s: %d events (%d mrs, %d issues, %d commits) in window [%s, %s]\n",
			project, len(all), len(mrs), len(issues), len(commits),
			since.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		return res, nil
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
	// The cap already ran inside fetchProjectEpics, before the
	// per-epic issue fetch, so there is nothing to trim here.
	events.AttachEpics(all, epics)
	res.EventsFetched = len(all)
	res.EventsUsed = min(len(all), p.Cfg.MaxEvents)
	// Carry the annotated slice so runEpicRollup can count each epic's
	// events exactly. Only retained when the project actually has
	// active epics; otherwise nothing downstream reads it.
	if len(epics) > 0 {
		res.AnnotatedEvents = all
	}

	// Record active epics for runEpicRollup (read by Run, not by the
	// per-project LLM call below). Allocated lazily so projects with
	// no epics don't add map entries.
	if len(epics) > 0 {
		p.lastActiveEpics[project] = epics
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
		period := periodSlug()
		res2, err := p.Publisher.Publish(publisher.Request{
			Name:    name,
			Body:    []byte(text),
			Force:   p.Opts.forceOverwrite(),
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

// audienceInputs is what one audience's rollup needs, or false when
// that audience has nothing to say (an all-team-only run has no
// customer rollup at all — publishing an empty one would put a file on
// the branch that says nothing).
type audienceInputs struct {
	name  string // prompt/label suffix: "rollup" / "epics-rollup"
	parts func(prompt.Audience) ([]rollupPart, error)
}

// rollupPart is one published file: a filename segment, a frontmatter
// title prefix, and the rendered prompt. A rollup that splits by
// customer returns one part per customer; one that doesn't returns a
// single part.
type rollupPart struct {
	name   string // filename kind segment, e.g. "epics-rollup", "epics-acme"
	title  string // frontmatter title prefix
	system string
	user   string
}

// eligibleProjects applies the D5 audience filter: the customer rollups
// see only projects that produced customer content, the team rollups see
// everything. Shared by both rollups so the security boundary lives in
// one place.
func eligibleProjects(aud prompt.Audience, projects []ProjectResult) []ProjectResult {
	if aud != prompt.AudienceCustomer {
		return projects
	}
	out := make([]ProjectResult, 0, len(projects))
	for _, pr := range projects {
		if pr.CustomerOn {
			out = append(out, pr)
		}
	}
	return out
}

// runAudienceRollups is the single rollup driver. runRollup and
// runEpicRollup differed only in how they built their per-audience
// inputs; the audience loop, the D5 filter, the guard call, the LLM
// call and the publish were verbatim copies. One loop now, so a fix to
// the audience boundary cannot land in one path and miss the other.
func (p *Pipeline) runAudienceRollups(ctx context.Context, projects []ProjectResult, spec audienceInputs) ([]string, error) {
	var files []string
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		// No eligible projects for this audience → no file. For the team
		// audience eligibleProjects is the whole run, so this only trips
		// on an empty run (both callers already guarantee ≥1).
		if len(eligibleProjects(aud, projects)) == 0 {
			continue
		}
		parts, err := spec.parts(aud)
		if err != nil {
			if errors.Is(err, errSkipAudience) {
				continue
			}
			return files, err
		}
		for _, part := range parts {
			label := spec.name + "/" + part.name
			if err := p.checkPrompt(ctx, label, aud, part.system+"\n\n"+part.user); err != nil {
				return files, err
			}
			resp, err := p.LLM.Complete(ctx, llm.Request{
				System:    part.system,
				User:      part.user,
				Model:     p.Opts.Model,
				MaxTokens: 2048,
			})
			if err != nil {
				return files, fmt.Errorf("llm %s %s: %w", label, aud, err)
			}
			res, err := p.Publisher.Publish(publisher.Request{
				Name:    rollupFilename(p.Opts.Since, aud, part.name),
				Body:    []byte(resp.Text),
				Force:   p.Opts.forceOverwrite(),
				Kind:    publisher.KindRollup,
				Period:  periodSlug(),
				Title:   part.title + " " + string(aud),
				Project: "",
				Public:  aud == prompt.AudienceCustomer,
			})
			if err != nil {
				return files, fmt.Errorf("write %s %s: %w", label, aud, err)
			}
			files = append(files, res.Ref)
		}
	}
	return files, nil
}

// runRollup runs one extra LLM call per audience over the per-project
// reports and writes the combined files. Only called when len(Projects)
// > 1. The rollup prompt takes per-project reports as input and asks the
// model to surface cross-project themes.
func (p *Pipeline) runRollup(ctx context.Context, projects []ProjectResult) ([]string, error) {
	return p.runAudienceRollups(ctx, projects, audienceInputs{
		name: "rollup",
		parts: func(aud prompt.Audience) ([]rollupPart, error) {
			// Pick the right per-audience text per project. Per-project
			// results carry both texts; we feed the rollup the same
			// audience the per-project report was written for.
			eligible := eligibleProjects(aud, projects)
			reports := make([]prompt.ProjectReport, 0, len(eligible))
			for _, pr := range eligible {
				body := pr.TeamText
				if aud == prompt.AudienceCustomer {
					body = pr.CustomerText
				}
				reports = append(reports, prompt.ProjectReport{Name: pr.Project, Body: body})
			}
			system, user, err := prompt.RenderRollup(aud, p.Opts.Since, reports)
			if err != nil {
				return nil, fmt.Errorf("render rollup %s: %w", aud, err)
			}
			return []rollupPart{{name: "rollup", title: "rollup", system: system, user: user}}, nil
		},
	})
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
// epicRollupPart is one project-scope group of epics destined for a
// single file: per-customer files carry CustomerCode, the combined file
// carries "".
type epicRollupPart struct {
	Customer string
	Entries  []prompt.EpicRollupEntry
}

// bucketEpicsByCustomer groups a run's active epics for the customer
// audience: one bucket per `org::CODE` epic label, plus one bucket for
// the epics nobody has labelled yet.
//
// The team audience is not split — internal readers want the whole
// picture, so its single combined part is built by bucketAllEpics.
//
// Buckets come back sorted by customer code so two runs over the same
// window produce the same file order on the branch. The unlabelled
// bucket sorts last: it is the backlog, not a customer.
func bucketEpicsByCustomer(projects []ProjectResult, window time.Duration) []epicRollupPart {
	byCode := make(map[string]*epicRollupPart)
	// Touch the combined bucket first so it always exists and always
	// appears last in the sorted output below.
	byCode[""] = &epicRollupPart{}

	for _, pr := range projects {
		if len(pr.ActiveEpics) == 0 {
			continue
		}
		perCode := make(map[string][]prompt.EpicSummary)
		for _, e := range pr.ActiveEpics {
			// Inheritance: org::* labels on the epic OR on its
			// child issues both attribute the epic to a customer.
			// An unlabelled epic with one child issue labelled
			// org::ACME is still an ACME epic.
			codes := policy.EpicCustomerCodes(e)
			// Counted once per epic, not once per customer bucket: it
			// is a property of the epic, not of the attribution.
			n := countEpicEvents(pr.AnnotatedEvents, e)
			if len(codes) == 0 {
				// No org:: anywhere → the unlabelled bucket. The
				// combined file becomes the "not yet attributed"
				// report rather than a silent miss.
				perCode[""] = append(perCode[""], prompt.EpicSummary{Epic: e, EventCount: n})
				continue
			}
			for _, code := range codes {
				perCode[code] = append(perCode[code], prompt.EpicSummary{Epic: e, EventCount: n})
			}
		}
		for code, summaries := range perCode {
			b := byCode[code]
			if b == nil {
				b = &epicRollupPart{Customer: code}
				byCode[code] = b
			}
			b.Entries = append(b.Entries, prompt.EpicRollupEntry{Project: pr.Project, Epics: summaries})
		}
	}

	out := make([]epicRollupPart, 0, len(byCode))
	for _, code := range sortedCustomerCodes(byCode) {
		b := byCode[code]
		if len(b.Entries) == 0 {
			continue
		}
		out = append(out, *b)
	}
	return out
}

// sortedCustomerCodes returns the bucket keys with the unlabelled
// bucket last and the customer codes in lexical order.
func sortedCustomerCodes(byCode map[string]*epicRollupPart) []string {
	keys := make([]string, 0, len(byCode))
	for k := range byCode {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return append(keys, "")
}

// bucketAllEpics puts every active epic in one part — the team
// audience's view, and the fallback when per-customer splitting is off.
func bucketAllEpics(projects []ProjectResult) []epicRollupPart {
	var entries []prompt.EpicRollupEntry
	for _, pr := range projects {
		if len(pr.ActiveEpics) == 0 {
			continue
		}
		summaries := make([]prompt.EpicSummary, 0, len(pr.ActiveEpics))
		for _, e := range pr.ActiveEpics {
			summaries = append(summaries, prompt.EpicSummary{
				Epic:       e,
				EventCount: countEpicEvents(pr.AnnotatedEvents, e),
			})
		}
		entries = append(entries, prompt.EpicRollupEntry{Project: pr.Project, Epics: summaries})
	}
	if len(entries) == 0 {
		return nil
	}
	return []epicRollupPart{{Entries: entries}}
}

func (p *Pipeline) runEpicRollup(ctx context.Context, projects []ProjectResult) ([]string, error) {
	// Pick a representative project name for the report header (the
	// rollup spans N projects; we surface a single project path in the
	// doc-builder frontmatter — first by sorted order for determinism).
	repProject := ""
	if len(projects) > 0 {
		repProject = projects[0].Project
	}
	return p.runAudienceRollups(ctx, projects, audienceInputs{
		name: "epics-rollup",
		parts: func(aud prompt.Audience) ([]rollupPart, error) {
			// The customer audience splits per customer; the team
			// audience stays whole.
			buckets := bucketAllEpics(eligibleProjects(aud, projects))
			if aud == prompt.AudienceCustomer {
				buckets = bucketEpicsByCustomer(eligibleProjects(aud, projects), p.Opts.Since)
			}
			if len(buckets) == 0 {
				// Nothing to talk about for this audience — the caller
				// skips publishing rather than writing an empty report.
				return nil, errSkipAudience
			}
			parts := make([]rollupPart, 0, len(buckets))
			for _, b := range buckets {
				system, user, err := prompt.RenderEpicRollup(aud, repProject, p.Opts.Since, b.Entries)
				if err != nil {
					return nil, fmt.Errorf("render epics rollup %s: %w", aud, err)
				}
				name, title := "epics-rollup", "epics rollup"
				if b.Customer != "" {
					name = "epics-" + slugCustomer(b.Customer)
					title = "epics rollup for " + b.Customer
				}
				parts = append(parts, rollupPart{name: name, title: title, system: system, user: user})
			}
			return parts, nil
		},
	})
}

// errSkipAudience tells runAudienceRollups that this audience produced
// no inputs, so no file should be published for it.
var errSkipAudience = errors.New("no inputs for this audience")

// countEpicEvents tallies how many events in the project's event set
// belong to this epic. Exact: AttachEpics annotates every issue event
// with the epic that contains it, so this counts real annotations
// rather than searching rendered text.
//
// The previous version counted occurrences of the epic's title in the
// rendered report body, which double-counted two epics sharing a title
// and could be inflated by the title appearing in prose — and the
// number reached customer-facing documents.
//
// events is the project's full annotated event slice; counting a
// subset (e.g. only the customer-audience events) would understate.
func countEpicEvents(events []events.NormalizedEvent, e *gitlab.Epic) int {
	if e == nil {
		return 0
	}
	n := 0
	for _, ev := range events {
		if ev.Epic != nil && ev.Epic.IID == e.IID {
			n++
		}
	}
	return n
}

// optOutReason reports why a project is excluded from the run, or ""
// when it should run. Two project-owner signals, either sufficient:
//
//   - the `no-summarize` topic — free, the topics already arrived with
//     the project summary, so it costs nothing and short-circuits the
//     file read below.
//   - a `.summaryignore` or `.docignore` file at the repo root — one
//     Files API call, or zero if the topic already excluded it.
//
// The topic is checked first purely because it is free, not because it
// wins: both signals mean "do not run me", so precedence only decides
// which reason gets logged. The reason is printed, because the topic is
// otherwise invisible to anyone who only looks at the repository.
func optOutReason(ctx context.Context, gc *gitlab.Client, path string, topics []string) (string, error) {
	if slices.Contains(topics, policy.TopicNoSummarize) {
		return "topic " + policy.TopicNoSummarize, nil
	}
	has, err := gc.HasOptOutFile(ctx, path)
	if err != nil {
		// 5xx/401/429 — real errors, fail the run. 404/403
		// soft-degrade inside HasOptOutFile to (false, nil) so moved
		// or confidential projects are included by default.
		return "", fmt.Errorf("opt-out check %s: %w", path, err)
	}
	if has {
		return "opt-out file", nil
	}
	return "", nil
}

// periodSlug returns the ISO-8601 week slug for the current period,
// e.g. "2026-W42". The GitLab publisher uses this as the
// source-branch suffix and the per-period directory, so every run
// within the same Mon→Sun week lands on the same branch and updates
// the same MR rather than forking a new one per invocation.
//
// This replaced a `--period-mode daily|monday` flag. A day-granular
// bucket can't be expressed as a week number — seven runs would
// collide on one directory — so the flag went away with its daily
// mode rather than leaving two modes that meant the same thing.
func periodSlug() string {
	return weekSlugFor(time.Now().UTC())
}

// weekSlugFor is periodSlug with the timestamp injected so the
// calendar arithmetic is unit-testable without freezing time.
func weekSlugFor(now time.Time) string {
	year, week := now.ISOWeek()
	// %02d on the week matters: Go's ISOWeek returns 1..53, but a
	// bare %d would render "2026-W1" and sort before "2026-W10"
	// for anything reading the directory as a string.
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// mondayAnchor returns the most-recent Monday at-or-before t. Calendar
// math: Weekday() is Sunday=0..Saturday=6, so we want the offset
// to be (Weekday() + 6) mod 7 (Monday=0). Subtract that many days.
func mondayAnchor(t time.Time) time.Time {
	weekday := int(t.Weekday())
	daysSinceMonday := (weekday + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-daysSinceMonday, 0, 0, 0, 0, t.Location())
}

// maxProjects resolves --max-projects. Zero or negative means "unset",
// which is the default cap — NOT "no limit". An explicit value is
// honoured even when it is below the default, so an operator can
// deliberately run a smaller set.
func (o Options) maxProjects() int {
	if o.MaxProjects > 0 {
		return o.MaxProjects
	}
	return DefaultMaxProjects
}

// forceOverwrite is always true. Every run lands in the same weekly
// bucket, so a refuse-first local publisher would fail the cron from
// day 2 onward with "file exists". The GitLab publisher accumulates
// onto the same branch regardless. Kept as a method rather than
// deleted so the call sites read the same and so a future mode that
// needs refuse-first has an obvious home.
func (o Options) forceOverwrite() bool {
	return true
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
	return checkPrompt(ctx, p.Guard, project+"/"+string(aud), prompt)
}

// checkPrompt is the single entry point for the injection guard. It is a
// free function rather than a Pipeline method because MonthlyRunner has
// no Pipeline but must be guarded too — `summarizer monthly` feeds last
// month's published text, which is itself LLM output derived from GitLab
// issue and MR bodies. An unguarded path makes `safety.enabled: true` a
// partial promise.
//
// label identifies the call in a log line ("group/proj/team",
// "monthly-rollup/2026-09/customer").
func checkPrompt(ctx context.Context, g safety.Guard, label, prompt string) error {
	if g == nil {
		// Defensive: a nil Guard is a wiring mistake, and panicking
		// here would surface as an unrelated crash mid-run. Noop is
		// the same behaviour as the guard being disabled, which is
		// what a nil here always means.
		g = safety.NoopGuard{}
	}
	verdict, score, err := g.Check(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "summarizer: WARN safety guard unavailable for %s — proceeding without check: %v\n", label, err)
		return nil
	}
	if verdict == safety.VerdictInjection {
		fmt.Fprintf(os.Stderr, "summarizer: safety guard REJECTED prompt for %s (score=%.3f >= threshold)\n", label, score)
		return fmt.Errorf("%w (score=%.3f): %s", ErrInjection, score, label)
	}
	fmt.Fprintf(os.Stderr, "summarizer: safety guard cleared %s (score=%.3f)\n", label, score)
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

// fillMRDetail fans out the per-MR /changes call across
// changeConcurrency workers and attaches the aggregate plus the
// truncated diff bodies onto each MergeRequest.
//
// One call per MR serves both needs. A failure on one MR is logged and
// that MR proceeds without changes data — one missing per-file summary
// doesn't fail the run. The prompts that include diff chunks use <data>
// markers and instruct the model to "infer what the MR does from the
// diff — paraphrase, never paste code into the report", which treats the
// diff as data, not instructions.
func fillMRDetail(ctx context.Context, gc *gitlab.Client, project string, mrs []gitlab.MergeRequest, wantDiffs bool) {
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
			summary, chunks, err := gc.FetchMRDetail(ctx, project, mrs[i].IID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "summarizer: changes fetch for MR %d failed: %v (skipping)\n", mrs[i].IID, err)
				return
			}
			mrs[i].Changes = &summary
			if wantDiffs {
				mrs[i].DiffChunks = chunks
			}
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
	// Sort by IID before anything else. GitLab makes no promise about
	// the order /epics returns, and this slice is rendered into the
	// prompt in this order — so without sorting, two runs over identical
	// input produce different documents and the fingerprint flags churn
	// that is not a real content change.
	sort.Slice(raw, func(i, j int) bool { return raw[i].IID < raw[j].IID })

	// Each epic's issues are one HTTP round-trip, so ordering of
	// operations matters more than it looks:
	//
	//  1. Cap FIRST. Fetching issues for all N epics and then dropping
	//     all but 20 made N sequential calls to produce a report that
	//     uses 20 of them — the dominant cost of a run on a group with
	//     many epics, and it grew every quarter.
	//  2. Then filter by activity, which needs the issues we fetched.
	//
	// The cap trades coverage for wall time: the epics that survive are
	// the 20 lowest IIDs rather than the 20 most recently active.
	// maxActiveEpics sits far above the usual active count so this
	// rarely bites, and the stderr line says so when it does.
	if len(raw) > maxActiveEpics {
		fmt.Fprintf(os.Stderr, "summarizer: %s: %d epics in group %s, considering the first %d by IID\n",
			project, len(raw), ns.FullPath, maxActiveEpics)
		raw = raw[:maxActiveEpics]
	}
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
	return resolveProjects(ctx, p.GitLab, p.Opts)
}

// resolveProjects merges explicit and discovered inputs. Takes the
// Client directly so unit tests don't need a full Pipeline.
func resolveProjects(ctx context.Context, gc *gitlab.Client, opts Options) ([]string, map[string]bool, error) {
	finalList := make([]string, 0, len(opts.Projects))
	discoveredSet := make(map[string]bool)
	included := 0
	excluded := 0

	// Explicit projects pass through the same opt-out gate as
	// discovered ones. They used to be appended unconditionally, which
	// meant an operator's `--project` silently overrode a project's own
	// `.summaryignore` — the opposite of what the comment below claimed
	// and of what the operator chose. The project-owner signal wins: a
	// cron line is not a licence to publish something the project asked
	// not to be published.
	for _, path := range opts.Projects {
		summary, err := gc.FetchProjectSummary(ctx, path)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch project %s: %w", path, err)
		}
		var topics []string
		if summary != nil {
			topics = summary.Topics
		}
		reason, err := optOutReason(ctx, gc, path, topics)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			excluded++
			fmt.Fprintf(os.Stderr, "  excluded (%s): %s\n", reason, path)
			continue
		}
		included++
		finalList = append(finalList, path)
	}

	if len(opts.DiscoveredGroups) == 0 && !opts.DiscoverByTopic && !opts.DiscoverActive {
		return finalList, discoveredSet, nil
	}

	since := time.Now().UTC().Add(-opts.Since)

	// Probe + classify + manifest one candidate. Shared by the group
	// scan and the server-wide topic scan; both produce the same
	// ProjectSummary and need the same activity gate.
	admit := func(proj gitlab.ProjectSummary) error {
		// Skip projects already explicit-listed (dedupe wins to
		// the explicit listing: FromDiscovery stays false).
		if containsString(opts.Projects, proj.Path) {
			return nil
		}
		// Project-level opt-out. Checked before the activity probe so
		// an opted-out project costs one API call and no probe at all.
		reason, err := optOutReason(ctx, gc, proj.Path, proj.Topics)
		if err != nil {
			return err
		}
		if reason != "" {
			excluded++
			fmt.Fprintf(os.Stderr, "  excluded (%s): %s\n", reason, proj.Path)
			return nil
		}
		active, err := gc.ProbeProjectActivity(ctx, proj.Path, since)
		if err != nil {
			// 4xx (other than 404 already softened) and 5xx fail
			// the whole run via the wrapped error. We could
			// soften further, but a probe-failure signals a real
			// transport / auth issue that's worth surfacing.
			return fmt.Errorf("probe %s: %w", proj.Path, err)
		}
		// Audience classification (D5): the default is team-only
		// for every project; `summarize` on the project is the
		// only way to produce customer-facing content. The
		// config/CLI override still wins when set, and
		// `summarize-team-only` continues to mean "team-only
		// even if the project has `summarize`."
		inOverride := containsString(opts.InternalOnlyProjects, proj.Path)
		aud := policy.Classify(proj.Topics, inOverride)
		tnoTag := ""
		if aud.TeamOnly {
			tnoTag = " (team-only)"
		}
		if !active {
			excluded++
			fmt.Fprintf(os.Stderr, "  excluded (no MRs, issues or commits in window): %s%s last_activity_at=%s\n",
				proj.Path, tnoTag, proj.LastActivityAt.Format(time.RFC3339))
			return nil
		}
		included++
		finalList = append(finalList, proj.Path)
		discoveredSet[proj.Path] = true
		fmt.Fprintf(os.Stderr, "  included:%s %s last_activity_at=%s\n",
			tnoTag, proj.Path, proj.LastActivityAt.Format(time.RFC3339))
		return nil
	}

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
			if err := admit(proj); err != nil {
				return nil, nil, err
			}
		}
	}

	// Server-wide scan: every project on the instance carrying any
	// of the topic markers, across all groups the token can see.
	if opts.DiscoverByTopic {
		projects, err := gc.FetchProjectsByTopic(ctx, discoveryTopics())
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery from topics=%v; scanned=%d\n",
			discoveryTopics(), len(projects))
		for _, proj := range projects {
			if err := admit(proj); err != nil {
				return nil, nil, err
			}
		}
	}

	// Default discovery: every project the token can see with
	// activity in the window. No topic, no config entry. A project
	// that was worked on gets a report; the ways out are the
	// .summaryignore / .docignore file or an explicit audience
	// override.
	if opts.DiscoverActive {
		projects, err := gc.FetchActiveProjects(ctx, since)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery by activity since=%s; scanned=%d\n",
			since.UTC().Format(time.RFC3339), len(projects))
		for _, proj := range projects {
			if err := admit(proj); err != nil {
				return nil, nil, err
			}
		}
	}

	fmt.Fprintf(os.Stderr, "summarizer: discovery summary included=%d excluded=%d\n", included, excluded)
	return finalList, discoveredSet, nil
}

// discoveryTopics is the topic set the server-wide scan looks for.
// Both markers, because either one is a valid "include me" signal —
// `summarize-team-only` alone means "run me, team audience only",
// which the policy classifier handles downstream.
func discoveryTopics() []string {
	return []string{policy.TopicSummarize, policy.TopicSummarizeTeamOnly}
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

// windowDays renders the window as the "Nd" segment every report
// filename carries. Sub-day windows round up to 1 so a --since 12h run
// doesn't produce a file named "--0d--".
func windowDays(window time.Duration) int {
	return max(int(window/(24*time.Hour)), 1)
}

// filename: <sanitized project>--<since-days>d--<audience>.md. The "since"
// is encoded in the filename so re-runs land on different files; combined
// with the refuse-first policy in writer, double-runs are safe.
func filename(window time.Duration, project string, aud prompt.Audience) string {
	return fmt.Sprintf("%s--%dd--%s.md", sanitize(project), windowDays(window), aud)
}

// rollupFilename is the combined-report name. kind is "rollup" or
// "epics-rollup"; the latter keeps its distinct prefix so a docbuilder
// operator browsing the period directory can tell the two apart without
// rendering either.
func rollupFilename(window time.Duration, aud prompt.Audience, kind string) string {
	return fmt.Sprintf("combined--%s--%dd--%s.md", kind, windowDays(window), aud)
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

// slugCustomer renders a customer code as a filename-safe segment:
// lowercased, alphanumerics kept, every other run collapsed to a single
// "-". "Acme Corp" → "acme-corp", "  ACME--EU  " → "acme-eu".
//
// Two codes that slug to the same string ("Acme Corp" and "acme-corp")
// would collide on the branch. GitLab labels are case-sensitive and an
// org::code is expected to be one canonical token, so this is a
// display convenience rather than an identity.
func slugCustomer(code string) string {
	var b strings.Builder
	prevDash := true // leading separators are dropped
	for _, r := range strings.ToLower(code) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
