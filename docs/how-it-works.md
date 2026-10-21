# How summarizer works

A single run fetches GitLab activity for one project (or more),
normalises it, renders a prompt, calls an LLM, and writes two Markdown
reports — one for customers, one for the team. This document maps
the data flow and the components.

## Data flow

```
┌─────────────────────────────────────────────────────────────────────┐
│ config (summarizer.yaml)   +   CLI flags   +   env vars (secrets)    │
└────────────────────────────────┬────────────────────────────────────┘
                                 │ resolveProjects()
                                 │  FetchActiveProjects  (default: every project with
                                 │    activity in the window, via last_activity_after)
                                 │  HasOptOutFile        (.summaryignore / .docignore
                                 │    at the repo root → skip the project entirely)
                                 │  ProbeProjectActivity (MRs → issues → commits;
                                 │    the MR probe answers most of the time)
                                 │  policy.Classify      (audience: team-only unless the
                                 │    project carries the `summarize` topic)
                                 ▼
                    ┌────────────────────────────────┐
                    │   per-project loop  (N times)   │
                    └─────────────┬──────────────────┘
                                  │
   ┌──────────────────────────────┴───────────────────────────────┐
   │ FetchMRs       (offset pagination; 429-aware backoff; retry)   │
   │ FetchMRDetail  (per-MR worker pool, 4 concurrent; one call     │
   │                serves both the +N/-M aggregate and --include-diffs)
   │ FetchIssues                                                   │
   │ FetchCommits                                                  │
   │ FetchProjectSummary  (project topics → audience policy)        │
   │ FetchProjectNamespace (auto-detects group kind/full_path)      │
   │ FetchEpics         ─┐ (group-scoped, Premium/Ultimate feature) │
   │ FetchEpicIssues      (per-epic issues pull; soft-degrades 403) │
   └──────────────────────────────┬───────────────────────────────┘
                                  │ []MergeRequest / []Issue / []Commit / []Epic
                                  ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  internal/events                                              │
   │  MergeRequest → events.MR  ─┐                                │
   │  Issue         → events.Issue│                                │
   │  Commit       → events.Commit│                                │
   │                              │                                │
   │  Converge to a single struct: events.NormalizedEvent         │
   │  (Kind, Action, Ref, Title, Author, URL, Labels, Comments,    │
   │   Created, Updated, Changes ← aggregated per-MR stats,        │
   │   TeamOnly ← MR/issue body, capped, only for team audience)   │
   └──────────────────────────────┬───────────────────────────────┘
                                  │ []NormalizedEvent (cap to max_events, drop oldest)
                                  ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  internal/prompt                                              │
   │  pick template: customer.tmpl OR team.tmpl                   │
   │  sort events (most-recent first, stable tiebreak)              │
   │  format each:                                                │
   │    - [MR #42] (merged, by alice) "..." [+412/-89 in 12] ─ url│
   │      body: <data>...</data>   ← team prompt only             │
   │      diff: <data>...</data>   ← --include-diffs + team only   │
   │  truncate to max_events                                      │
   │  render template → (system, user)                             │
   └──────────────────────────────┬───────────────────────────────┘
                                  │ (system + user)
                                  ▼
            ┌─────────────────────────────────────────┐
            │ internal/safety  (opt-in, --safety-enabled)│
            │ guard.Check(system + user)                │
            │  - verdict "injection" → HARD-FAIL       │
            │  - verdict "safe"      → continue        │
            │  - guard error        → SOFT-FAIL (WARN) │
            └─────────────────────┬───────────────────┘
                                  │ (only on "safe")
                                  ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  internal/llm                                                 │
   │  POST {BaseURL}/v1/chat/completions   (OpenAI-compatible)    │
   │  OR POST {BaseURL}/v1/messages          (Anthropic)            │
   │  Returns rendered Markdown + token usage                       │
   └──────────────────────────────┬───────────────────────────────┘
                                  │ rendered Markdown (raw)
                                  ▼
   ┌───────────────────────────────────────────────────────────────┐
   │  internal/publisher                                           │
   │  Local: os.WriteFile(tmp) → os.Rename(target)                 │
   │         (overwrite-in-place; week buckets make refuse-first   │
   │          unusable)                                            │
   │  GitLab:                                                    │
   │    1. ensure branch <prefix>/<YYYY-Www>  (GET first, POST 404)│
   │    2. GET file → existing UID + last_commit_id               │
   │    3. POST file if 404 (create) / PUT file if 200 (update)   │
   │    4. GET /merge_requests?source_branch=…&state=opened        │
   │    5. if none: POST /merge_requests                          │
   └───────────────────────────────────────────────────────────────┘
                                  │ files on disk OR commits on a branch
                                  ▼
                         ┌────────────────────────┐
                         │  Output                 │
                         │  Local or GitLab MR     │
                         └────────────────────────┘
```

## Component responsibilities

| Package | Owns |
|---|---|
| `cmd/summarizer` | Entry point. Subcommand routing (`run`, `monthly`, `version`), flag parsing, env-fallback for secrets, config + publisher + safety wiring. |
| `internal/config` | YAML loader; defaults; CLI-override merge; per-field validation. |
| `internal/gitlab` | `net/http` GitLab REST v4 client. MRs, issues, commits, project summary/namespace, epics + epic issues, the opt-out-file read, and the activity-default project scan. Offset pagination (`X-Next-Page`), 429-aware backoff with ±20% jitter, request caps. |
| `internal/events` | `NormalizedEvent` type. The single shape that crosses the port boundary between GitLab (any-specific) and the prompt builder (clean). All GitLab-JSON quirks stay in this package. `AttachEpics` mutates Issue events in place to attach their containing epic (`Epic *gitlab.Epic`); `FilterActiveEpics(window)` filters the pipeline's epic list to those with at least one issue updated within the window. |
| `internal/policy` | Pure classifiers. `Classify` decides audience from topics + operator override; `CustomerCodes` / `EpicCustomerCodes` extract `org::CODE` labels (the latter unioning an epic's own labels with its child issues'). No I/O, so it's table-tested and cheap to call. |
| `internal/prompt` | `Render(...)` for per-project reports, `RenderRollup` for the cross-project rollup, `RenderEpicRollup` for the epic rollup, `RenderMonthlyRollup` for the month-end summary. Eight embedded `.tmpl` files plus per-audience system prompts. |
| `internal/llm` | `Provider` interface. OpenAI Chat Completions + Anthropic Messages adapters. Pluggable — adding a third provider is one new file. |
| `internal/publisher` | `Publisher` interface (`Publish`). `Local` (atomic disk write) + `GitLab` (branch + Files API + MR, with a read-back path used by `summarizer monthly`). Frontmatter generation in `frontmatter.go`. |
| `internal/safety` | `Guard` interface. `DecideGuard` wraps `inful/decide` for pre-LLM prompt-injection checks. `NoopGuard` is the default-off path. `FakeGuard` for tests. |
| `internal/run` | The pipeline. `resolveProjects` does discovery + opt-out + audience classification. `runOneProject` does fetch + normalise + 2× LLM + 2× publish per project. `runRollup` / `runEpicRollup` share one audience loop. `MonthlyRunner` is the separate `summarizer monthly` path, which reads last month's published weekly rollups instead of re-fetching GitLab. |

## What stays out of scope

See [`docs/deferred.md`](deferred.md) for the canonical list. Summarizer's
posture on the items below hasn't changed; the full rationale lives in
deferred.md alongside the "not yet built" backlog and the "decided
against" log.

- Real-time activity streaming (we poll, not subscribe)
- Operator UI / control plane (everything is config + cron)
- Multi-tenant isolation (one binary, one operator, one vault)
- Webhook-driven runs (we're cron-driven; `--safety-enabled` is the only asynchronous guard)

## Enrollment is automatic now

An earlier draft of this document listed "automatic enrollment of new
projects" as out of scope, because projects had to be listed or
topic-tagged. That is no longer true: discovery defaults to every
project the token can see that had activity in the window, so a new
repo starts producing reports the week it is first pushed to. Projects
opt out with a `.summaryignore` or `.docignore` file at their repo
root. The only curation left is the optional `summarize` topic, which
decides whether a project also gets customer-facing output.
