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
	Projects             []string  // required (unless DiscoveredGroups is set): one or more GitLab paths "group/subgroup/proj"
	Since                time.Time // window start: Monday 00:00 UTC of the current ISO week
	Period               string    // the same week's slug, e.g. "2026-W43". Derived from the same instant as Since, so a run cannot file a week under the wrong directory.
	Provider             string    // "openai" or "anthropic" (overrides config)
	Model                string    // overrides config
	DryRun               bool      // fetch + print + exit; no LLM call, no file write
	IncludeDiffs         bool      // --include-diffs: fetch per-MR diff bodies and feed them into the team prompt
	DiscoveredGroups     []string  // --discover-group: each entry is a group path; the pipeline fetches the group's projects, probes each for activity, and includes the active ones in the run. Composes with Projects.
	InternalOnlyProjects []string  // --internal-only-project (repeatable) + YAML internal_only_projects block. Union with topic-based "summarize-team-only" classification; team-only projects produce no customer-facing report.
	DiscoverByTopic      bool      // --discover-by-topic: scan every project on the instance carrying a `summarize*` topic, across all groups the token can see. Composes with Projects and DiscoveredGroups.
	DiscoverActive       bool      // --discover-active: include every project the token can see that had activity since the window opened. Set automatically when no other discovery input is given, which is the documented default: an active project runs unless it opts out.
	MaxProjects          int       // --max-projects: hard cap on projects fed to the LLM, applied after discovery and opt-out but before any LLM call. Each project costs two completions, so a quarter where 200 repos were touched is ~400 calls; this bounds the bill. 0 = DefaultMaxProjects.
	PublishTarget        string    // the reports project summarizer writes into; discovery skips it. See config.PublishTarget, which returns "" when there is nothing to skip.
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
	Project       string
	EventsFetched int
	EventsUsed    int
	CustomerText  string // rendered markdown for the customer audience (empty when TeamOnly)
	TeamText      string // rendered markdown for the team audience
	FilesWritten  []string
	// FilesClosed counts writes whose period was already published, so the
	// file landed on a branch that will never merge again. Without it the
	// summary says "ok — 4 file(s) written" for a re-run that published
	// nothing to the default branch.
	FilesClosed      int
	TokensByAudience map[string]llm.Usage
	FromDiscovery    bool           // surfaced by --discover-group / --discover-by-topic
	TeamOnly         bool           // true ⇒ no customer-facing content was produced for this project
	CustomerOn       bool           // true ⇒ customer-facing content was produced (== !TeamOnly)
	ActiveEpics      []*gitlab.Epic // active epics for this project (had issue activity in window); used by runEpicRollup
	// EpicEventCounts maps epic IID to the number of events in this
	// project that belong to it, counted from the real annotations
	// AttachEpics produced.
	//
	// A map rather than the annotated event slice itself: the slice is
	// every MR, issue and commit the project had (pre-cap, so it can be
	// thousands), and holding one per project for the whole run to
	// produce twenty integers is memory that only grows. This carries
	// the same information in a fraction of the space.
	EpicEventCounts map[int]int
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

	// Resolution totals from the last resolveProjects call, so the
	// "nothing to run" line can report real numbers instead of the
	// hardcoded zeros it used to print — which contradicted the
	// per-project exclusion lines printed immediately above it.
	lastIncluded int
	lastExcluded int
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
	p.lastIncluded, p.lastExcluded = 0, 0

	if len(p.Opts.Projects) == 0 && len(p.Opts.DiscoveredGroups) == 0 && !p.Opts.DiscoverByTopic && !p.Opts.DiscoverActive {
		return res, errors.New("at least one --project, --discover-group, --discover-by-topic or --discover-active is required")
	}

	finalList, discoveredSet, err := p.resolveProjects(ctx)
	if err != nil {
		return res, err
	}
	if len(finalList) == 0 {
		// Discovery ran and found nothing runnable. The per-project
		// lines above already say why each one was dropped, so this
		// only needs to state the totals — and it must use the real
		// ones, or it contradicts the lines immediately above it.
		fmt.Fprintf(os.Stderr, "summarizer: nothing to run — %d candidate(s) excluded, 0 included; see the lines above\n",
			p.lastExcluded)
		return res, nil
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

	// Order the rollup input LEAST-active first.
	//
	// Both rollup budgets (prompt.RenderRollup and prompt.capEpicEntries)
	// drop from the front of this slice when the prompt exceeds its
	// budget, so "the front" decides whose work is omitted. The slice
	// arriving here was explicit --project entries followed by
	// discovered projects in last_activity_at DESCENDING order — newest
	// activity first — so "drop the first" meant "drop the busiest",
	// while both functions documented it as dropping the oldest.
	//
	// EventsFetched is the project's activity in the window, already
	// on ProjectResult, so ordering by it makes the policy legible:
	// when the budget bites, the quiet projects go. Path breaks ties,
	// so the cohort is reproducible.
	sort.SliceStable(res.Projects, func(i, j int) bool {
		if res.Projects[i].EventsFetched != res.Projects[j].EventsFetched {
			return res.Projects[i].EventsFetched < res.Projects[j].EventsFetched
		}
		return res.Projects[i].Project < res.Projects[j].Project
	})

	if len(res.Projects) > 0 && !p.Opts.DryRun {
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

	inOverrideList := slices.Contains(p.Opts.InternalOnlyProjects, project)
	topics := p.projectTopics(ctx, project)
	audience := policy.Classify(topics, inOverrideList)
	res.TeamOnly = audience.TeamOnly
	res.CustomerOn = audience.CustomerOn

	since := p.Opts.Since

	mrs, err := p.GitLab.FetchMRs(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch mrs: %w", err)
	}
	issues, err := p.GitLab.FetchIssues(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch issues: %w", err)
	}
	commits, err := p.GitLab.FetchCommits(ctx, project, since)
	if err != nil {
		return res, fmt.Errorf("fetch commits: %w", err)
	}

	// Dry-run exits HERE, before every PER-ITEM fetch. The three list
	// calls above are one request each and their counts are exactly
	// what the preview reports. Everything below costs one request per
	// item: /changes per merge request, then /issues per epic. Paying
	// those for output the run discards makes the preview slower than
	// the run it previews, which is how you stop people previewing.
	if p.Opts.DryRun {
		total := len(mrs) + len(issues) + len(commits)
		res.EventsFetched = total
		res.EventsUsed = min(total, p.Cfg.MaxEvents)
		fmt.Fprintf(os.Stderr, "summarizer dry-run: %s: %d events (%d mrs, %d issues, %d commits) in window [%s, %s]\n",
			project, total, len(mrs), len(issues), len(commits),
			since.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
		return res, nil
	}

	// One /changes call per MR serves both needs. With --include-diffs
	// the same response carries the diff bodies the team prompt wants;
	// without it only the aggregate is used, but there is still no
	// reason to make the call twice.
	//
	// Bounded by MaxEvents BEFORE the fetch. capEvents (inside
	// prompt.Render) keeps only the newest N events, so a merge request
	// that will be dropped anyway was costing a full /changes request
	// to be thrown away — and max_events is the flag an operator
	// reaches for to bound exactly this.
	if max := p.Cfg.MaxEvents; max > 0 && len(mrs) > max {
		var dropped int
		mrs, dropped = keepNewestMRs(mrs, max)
		fmt.Fprintf(os.Stderr, "summarizer: %s: %d merge request(s) in window, fetching changes for the %d most recent (--max-events %d)\n",
			project, max+dropped, max, max)
	}
	fillMRDetail(ctx, p.GitLab, project, mrs, p.Opts.IncludeDiffs)

	// Assembled after the trim and the fill, not before: every event the
	// prompt can show must carry the details that were just fetched.
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
	// The cap already ran inside fetchProjectEpics, before the
	// per-epic issue fetch, so there is nothing to trim here.
	events.AttachEpics(all, epics)
	res.EventsFetched = len(all)
	res.EventsUsed = min(len(all), p.Cfg.MaxEvents)
	// Count each epic's events now, while `all` is in scope, so the
	// epic rollup gets exact numbers without the event slice having to
	// outlive this function.
	if len(epics) > 0 {
		res.EpicEventCounts = epicEventCounts(all, epics)
	}

	// Record active epics for runEpicRollup (read by Run, not by the
	// per-project LLM call below). Allocated lazily so projects with
	// no epics don't add map entries.
	if len(epics) > 0 {
		p.lastActiveEpics[project] = epics
	}

	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		if aud == prompt.AudienceCustomer && !res.CustomerOn {
			// Audience policy (D5) says team-only. Skip the
			// customer iteration entirely: no LLM call, no file
			// write, no event rendering for the customer side.
			// Team iteration runs as usual.
			continue
		}

		system, user, err := prompt.Render(aud, project, all, p.Cfg.MaxEvents, epics)
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

		name := filename(project, aud)
		period := p.Opts.Period
		res2, err := p.Publisher.Publish(ctx, publisher.Request{
			Name:     name,
			Body:     []byte(text),
			Kind:     publisher.KindPerProject,
			Period:   period,
			Title:    string(aud),
			Audience: string(aud),
			Project:  project,
			Public:   aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return res, fmt.Errorf("write %s: %w", aud, err)
		}
		res.FilesWritten = append(res.FilesWritten, res2.Ref)
		if res2.Closed {
			res.FilesClosed++
		}
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
			res, err := p.Publisher.Publish(ctx, publisher.Request{
				Name:     rollupFilename(aud, part.name),
				Body:     []byte(resp.Text),
				Kind:     publisher.KindRollup,
				Period:   p.Opts.Period,
				Title:    part.title + " " + string(aud),
				Audience: string(aud),
				Project:  "",
				Public:   aud == prompt.AudienceCustomer,
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
			system, user, err := prompt.RenderRollup(aud, reports)
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
// BUG TRAP for the per-epic issue list (not yet built). An epic reaches
// MORE THAN ONE bucket, on purpose: policy.EpicCustomerCodes unions the
// epic's own org:: labels with those of its child issues, so an epic
// tagged org::ACME carrying a GLOBEX-tagged issue appears in both ACME's
// file and GLOBEX's. That is safe today only because the templates
// render epic-level fields alone. The moment a template starts printing
// Epic.Issues, the issues arrive unfiltered and ACME's customer report
// lists GLOBEX's tasks. Filter e.Issues to the bucket's code when
// building EpicSummary, before that work lands.
//
// The team audience is not split — internal readers want the whole
// picture, so its single combined part is built by bucketAllEpics.
//
// Buckets come back sorted by customer code so two runs over the same
// window produce the same file order on the branch. The unlabelled
// bucket sorts last: it is the backlog, not a customer.
func bucketEpicsByCustomer(projects []ProjectResult) []epicRollupPart {
	// No "" bucket. An epic with no org::CODE label on it or on any of
	// its issues is UNATTRIBUTED, and this function only feeds the
	// customer audience, which is published with public: true. Every
	// other audience decision in summarizer is opt-in — a project needs
	// the `summarize` topic, an epic needs its org:: label — so an
	// unattributed epic reaching a customer unconditionally was the
	// one place the default ran permissive. Attributing an epic to a
	// customer is a deliberate act; unattributed means nobody has made
	// it.
	//
	// Unattributed epics are not lost: the team audience gets the whole
	// fleet through bucketAllEpics, unfiltered. They show up there and
	// nowhere else, which is the correct place for work nobody has
	// decided is customer-visible.
	byCode := make(map[string]*epicRollupPart)

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
			n := pr.EpicEventCounts[e.IID]
			if len(codes) == 0 {
				// No org:: anywhere → not this audience's business.
				// Skipped rather than filed under a catch-all; see
				// the note above.
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

// sortedCustomerCodes returns the bucket keys in lexical order, so two
// runs over identical input produce identical documents.
func sortedCustomerCodes(byCode map[string]*epicRollupPart) []string {
	keys := make([]string, 0, len(byCode))
	for k := range byCode {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
				EventCount: pr.EpicEventCounts[e.IID],
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
	// No representative project name: the epic rollups name every
	// project in the body and the frontmatter carries Project: "",
	// because they span N projects. A "first project for determinism"
	// used to be threaded into the prompt data and rendered by nothing.
	return p.runAudienceRollups(ctx, projects, audienceInputs{
		name: "epics-rollup",
		parts: func(aud prompt.Audience) ([]rollupPart, error) {
			// The customer audience splits per customer; the team
			// audience stays whole.
			buckets := bucketAllEpics(eligibleProjects(aud, projects))
			if aud == prompt.AudienceCustomer {
				buckets = bucketEpicsByCustomer(eligibleProjects(aud, projects))
			}
			if len(buckets) == 0 {
				// Nothing to talk about for this audience — the caller
				// skips publishing rather than writing an empty report.
				return nil, errSkipAudience
			}
			parts := make([]rollupPart, 0, len(buckets))
			for _, b := range buckets {
				system, user, err := prompt.RenderEpicRollup(aud, b.Entries)
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

// epicEventCounts maps each epic IID to the number of events belonging
// to it. Exact: AttachEpics annotates every issue event with the epic
// that contains it, so this counts real annotations rather than
// searching rendered text.
//
// The previous implementation counted occurrences of the epic's title in
// the rendered report body, which double-counted two epics sharing a
// title and could be inflated by the title appearing in prose — and the
// number reached customer-facing documents.
//
// One pass over the events serves every epic, where counting per epic
// was O(epics x events).
func epicEventCounts(evs []events.NormalizedEvent, epics []*gitlab.Epic) map[int]int {
	counts := make(map[int]int, len(epics))
	for _, e := range epics {
		if e != nil {
			counts[e.IID] = 0
		}
	}
	for _, ev := range evs {
		if ev.Epic == nil {
			continue
		}
		if _, tracked := counts[ev.Epic.IID]; tracked {
			counts[ev.Epic.IID]++
		}
	}
	return counts
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

// weekSlugFor returns the ISO-8601 week slug for a period, e.g.
// "2026-W42". The GitLab publisher uses this as the source-branch
// suffix and the per-period directory, so every run within the same
// Mon→Sun week lands on the same branch and updates the same MR rather
// than forking a new one per invocation.
//
// It replaced a `--period-mode daily|monday` flag. A day-granular
// bucket can't be expressed as a week number — seven runs would
// collide on one directory — so the flag went away with its daily mode
// rather than leaving two modes that meant the same thing.
//
// Called once, at start-up, from the same instant that produced
// Options.Since — not once per publish. A run crossing Sunday midnight
// would otherwise compute a window from one week and file the files
// under the next.
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
// WeekSlug returns the ISO-8601 week slug for the period containing
// t, e.g. "2026-W43". Paired with WeekStart from the same instant.
func WeekSlug(t time.Time) string { return weekSlugFor(t) }

// WeekStart returns Monday 00:00 UTC of the ISO week containing t.
// Reports are filed under a week-number directory, so the window and the
// directory have to agree. A rolling 7 days does not: run on a Monday
// and the file named 2026-W43 holds nothing from W43 and a full week of
// W42, while on a Thursday the same command looks three weeks into the
// past. Monday 00:00 UTC is the ISO week's start, matching ISOWeek().
//
// The duration is evaluated once at start-up and then behaves like any
// other window. There is no second window and no flag to ask for one.
func WeekStart(now time.Time) time.Time {
	return mondayAnchor(now).UTC()
}

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
// keepNewestMRs trims an over-long merge-request list to the n most
// recently updated, returning the kept slice and how many were dropped.
//
// Sorted by events.MR's timestamp, not by MergeRequest.UpdatedAt. The
// event carries merged_at for a merged MR and closed_at for a closed
// one, and prompt.capEvents sorts on that. GitLab also bumps
// updated_at when a label changes, so sorting on it here disagreed
// with the cap: a housekeeping label sweep over 150 archived MRs made
// all 150 look recent, and the trim then discarded merges the cap
// would have kept. IID is the tiebreak for both.
//
// shortcut: builds one NormalizedEvent per MR to read one field. At
// maxEvents-scale that is a few hundred short-lived structs; revisit
// only if the list ever stops being bounded before this call.
func keepNewestMRs(mrs []gitlab.MergeRequest, n int) ([]gitlab.MergeRequest, int) {
	sorted := make([]gitlab.MergeRequest, len(mrs))
	copy(sorted, mrs)
	eventTime := make(map[int]time.Time, len(sorted))
	for _, m := range sorted {
		eventTime[m.IID] = events.MR(m).Updated
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if ti, tj := eventTime[sorted[i].IID], eventTime[sorted[j].IID]; !ti.Equal(tj) {
			return ti.After(tj)
		}
		return sorted[i].IID < sorted[j].IID
	})
	if len(sorted) <= n {
		return sorted, 0
	}
	return sorted[:n], len(sorted) - n
}

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
	// Two different orders, for two different jobs.
	//
	// SELECTION, by recency. The cap below decides which epics get their
	// issues fetched, and that fetch is what decides whether an epic is
	// active. Sorting by IID meant the cap took the 20 OLDEST epics and
	// then filtered those by activity — so a group whose recent epics all
	// have high IIDs reported almost nothing, because the active ones
	// were never fetched and so could not be reported at all.
	// `updated_at` arrives free in this same response, so selecting by
	// recency costs nothing.
	//
	// RENDERING, by IID. GitLab promises no order for /epics and the
	// surviving slice goes into the prompt as-is, so a stable order is
	// what keeps two runs over identical input producing identical
	// documents, and therefore an identical fingerprint.
	//
	// IID is the tiebreak in both, so nothing depends on GitLab's
	// response order.
	byRecency := func(i, j int) bool {
		if !raw[i].UpdatedAt.Equal(raw[j].UpdatedAt) {
			return raw[i].UpdatedAt.After(raw[j].UpdatedAt)
		}
		return raw[i].IID < raw[j].IID
	}
	// If the API omits updated_at every timestamp is the zero time,
	// every comparison ties, and the IID tiebreak hands back the OLDEST
	// epics — silently reintroducing the bug the recency sort exists to
	// fix, while the log line below still claims "most recently
	// updated". Fall back to the highest IIDs, which are newer by
	// construction, and say what happened.
	zeroTimestamps := true
	for _, e := range raw {
		if !e.UpdatedAt.IsZero() {
			zeroTimestamps = false
			break
		}
	}
	if zeroTimestamps {
		if len(raw) > maxActiveEpics {
			fmt.Fprintf(os.Stderr, "summarizer: %s: group %s reported no updated_at for any epic; "+
				"considering the %d highest-numbered instead\n", project, ns.FullPath, maxActiveEpics)
			raw = raw[len(raw)-maxActiveEpics:]
		}
	} else {
		sort.Slice(raw, byRecency)
		if len(raw) > maxActiveEpics {
			fmt.Fprintf(os.Stderr, "summarizer: %s: %d epics in group %s, considering the %d most recently updated\n",
				project, len(raw), ns.FullPath, maxActiveEpics)
			raw = raw[:maxActiveEpics]
		}
	}
	// Back to a stable render order for the prompt.
	sort.Slice(raw, func(i, j int) bool { return raw[i].IID < raw[j].IID })

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
// policy.Classify.
//
// shortcut: resolveProjects already holds these topics from the
// discovery scan, so this is one redundant GET per project (~50 on a
// capped run). Carrying them through would mean a fifth return value
// on resolveProjects plus plumbing into runOneProject. Revisit if the
// scan ever grows to hundreds of projects or the topic list becomes
// dynamic.
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
	list, discovered, excluded, err := resolveProjects(ctx, p.GitLab, p.Opts)
	p.lastExcluded = excluded
	return list, discovered, err
}

// resolveProjects merges explicit and discovered inputs. Takes the
// Client directly so unit tests don't need a full Pipeline.
func resolveProjects(ctx context.Context, gc *gitlab.Client, opts Options) ([]string, map[string]bool, int, error) {
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
			return nil, nil, excluded, fmt.Errorf("fetch project %s: %w", path, err)
		}
		var topics []string
		if summary != nil {
			topics = summary.Topics
		}
		reason, err := optOutReason(ctx, gc, path, topics)
		if err != nil {
			return nil, nil, excluded, err
		}
		if reason != "" {
			excluded++
			fmt.Fprintf(os.Stderr, "  excluded (%s): %s\n", reason, path)
			continue
		}
		included++
		finalList = append(finalList, path)
	}

	if len(opts.Projects) > opts.maxProjects() {
		// An explicit --project is a statement of intent; the cap is a
		// cost guard for a wide discovery sweep. They must not compete
		// in one alphabetical pool, or sorting would silently evict the
		// project someone named deliberately. Exceed the cap instead,
		// and say so.
		fmt.Fprintf(os.Stderr, "summarizer: %d explicit project(s) exceed --max-projects %d; "+
			"running all of them — raise the flag to change this\n", len(opts.Projects), opts.maxProjects())
	}

	if len(opts.DiscoveredGroups) == 0 && !opts.DiscoverByTopic && !opts.DiscoverActive {
		return finalList, discoveredSet, excluded, nil
	}

	since := opts.Since

	// Probe + classify + manifest one candidate. Shared by the group
	// scan and the server-wide topic scan; both produce the same
	// ProjectSummary and need the same activity gate.
	admit := func(proj gitlab.ProjectSummary) error {
		// Skip projects already explicit-listed (dedupe wins to
		// the explicit listing: FromDiscovery stays false).
		if slices.Contains(opts.Projects, proj.Path) {
			return nil
		}
		// Skip the reports project. Its merge requests ARE summarizer's
		// publishing MRs, so summarising it spends an LLM call every
		// week describing last week's reports. Placed after the
		// explicit-listing check so `--project <reports repo>` still
		// runs — an explicit listing is the operator overriding a
		// default, same authority as setting publish.project.
		if opts.PublishTarget != "" && proj.Path == opts.PublishTarget {
			excluded++
			fmt.Fprintf(os.Stderr, "  excluded (publish target): %s\n", proj.Path)
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
		inOverride := slices.Contains(opts.InternalOnlyProjects, proj.Path)
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

	// admitUpTo is the shared admission budget for every discovery
	// loop. Admit is ~6 sequential round-trips per project (opt-out file
	// read plus the activity probe), so stopping early is what keeps a
	// wide instance affordable.
	//
	// The budget counts INCLUSIONS, not candidates. Counting candidates
	// meant an instance whose most-recently-active projects were all
	// opted out or quiet examined `maxProjects` of them, included none,
	// and published nothing — while the log reported the rest as "not
	// considered", as though the cap were working as intended.
	admitUpTo := func(projects []gitlab.ProjectSummary, why string) error {
		// Most-recently-active first. last_activity_at is already in
		// this payload, so ranking is free.
		sort.SliceStable(projects, func(i, j int) bool {
			if !projects[i].LastActivityAt.Equal(projects[j].LastActivityAt) {
				return projects[i].LastActivityAt.After(projects[j].LastActivityAt)
			}
			return projects[i].Path < projects[j].Path
		})
		limit := opts.maxProjects()
		for n, proj := range projects {
			if len(finalList) >= limit {
				fmt.Fprintf(os.Stderr, "summarizer: %s budget reached (--max-projects %d, %d in the run); "+
					"%d further candidate(s) not examined\n", why, limit, len(finalList), len(projects)-n)
				return nil
			}
			if err := admit(proj); err != nil {
				return err
			}
		}
		return nil
	}

	for _, group := range opts.DiscoveredGroups {
		// v1: recursive=false. The recursive branch is reserved
		// for a future change; see FetchGroupProjects.
		projects, err := gc.FetchGroupProjects(ctx, group, false)
		if err != nil {
			return nil, nil, excluded, fmt.Errorf("discover group %s: %w", group, err)
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery from group=%s recursive=false; scanned=%d\n",
			group, len(projects))
		if err := admitUpTo(projects, "group "+group); err != nil {
			return nil, nil, excluded, err
		}
	}

	// Server-wide scan: every project on the instance carrying any
	// of the topic markers, across all groups the token can see.
	if opts.DiscoverByTopic {
		projects, err := gc.FetchProjectsByTopic(ctx, discoveryTopics())
		if err != nil {
			return nil, nil, excluded, err
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery from topics=%v; scanned=%d\n",
			discoveryTopics(), len(projects))
		if err := admitUpTo(projects, "topic scan"); err != nil {
			return nil, nil, excluded, err
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
			return nil, nil, excluded, err
		}
		fmt.Fprintf(os.Stderr, "summarizer: discovery by activity since=%s; scanned=%d\n",
			since.UTC().Format(time.RFC3339), len(projects))
		if err := admitUpTo(projects, "activity scan"); err != nil {
			return nil, nil, excluded, err
		}
	}

	fmt.Fprintf(os.Stderr, "summarizer: discovery summary included=%d excluded=%d\n", included, excluded)
	return finalList, discoveredSet, excluded, nil
}

// discoveryTopics is the topic set the server-wide scan looks for.
// Both markers, because either one is a valid "include me" signal —
// `summarize-team-only` alone means "run me, team audience only",
// which the policy classifier handles downstream.
func discoveryTopics() []string {
	return []string{policy.TopicSummarize, policy.TopicSummarizeTeamOnly}
}

// windowDays renders the window as the "Nd" segment every report
// filename carries.
// doesn't produce a file named "--0d--".
// filename and rollupFilename carry the constant "wk" window token.
// There is only one window — the current ISO week — so the token
// records nothing that the period directory does not already say. It
// is kept because the filenames were designed around it and docbuilder
// may key on it; a second window is not a thing this tool has.
func filename(project string, aud prompt.Audience) string {
	return fmt.Sprintf("%s--wk--%s.md", sanitize(project), aud)
}

// rollupFilename is the combined-report name. kind is "rollup" or
// "epics-rollup"; the latter keeps its distinct prefix so a docbuilder
// operator browsing the period directory can tell the two apart without
// rendering either.
func rollupFilename(aud prompt.Audience, kind string) string {
	return fmt.Sprintf("combined--%s--wk--%s.md", kind, aud)
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
