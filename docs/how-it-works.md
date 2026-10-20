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
                                 │ resolveOptions()
                                 ▼
                    ┌────────────────────────────────┐
                    │   per-project loop  (N times)   │
                    └─────────────┬──────────────────┘
                                  │
   ┌──────────────────────────────┴───────────────────────────────┐
   │ FetchMRs       (offset pagination; 429-aware backoff; retry)   │
   │ FetchMRChanges (per-MR worker pool, 4 concurrent; 50 MRs ≈ 50s)│
   │ FetchIssues                                                   │
   │ FetchCommits                                                  │
   └──────────────────────────────┬───────────────────────────────┘
                                  │ []MergeRequest / []Issue / []Commit
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
   │  GitLab:                                                    │
   │    1. ensure branch <prefix>/<period>                         │
   │    2. PUT file (base64 content + frontmatter wrapped)         │
   │    3. GET /merge_requests?source_branch=…&state=opened        │
   │    4. if none: POST /merge_requests                          │
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
| `cmd/summarizer` | Entry point. Subcommand routing, flag parsing, env-fallback for secrets, config + publisher + safety wiring. |
| `internal/config` | YAML loader; defaults; CLI-override merge; per-field validation. |
| `internal/gitlab` | `net/http` GitLab REST v4 client. Three fetchers (MRs, issues, commits) + one detail-fetcher (`/changes`). Offset pagination (`X-Next-Page`), 429-aware backoff with ±20% jitter, request caps. |
| `internal/events` | `NormalizedEvent` type. The single shape that crosses the port boundary between GitLab (any-specific) and the prompt builder (clean). All GitLab-JSON quirks stay in this package. |
| `internal/prompt` | `Render(audience, project, window, events, cap) → (system, user)`. Two embedded `.tmpl` files plus per-audience system prompts. `RenderRollup(...)` for the multi-project cross-rollup. |
| `internal/llm` | `Provider` interface. OpenAI Chat Completions + Anthropic Messages adapters. Pluggable — adding a third provider is one new file. |
| `internal/publisher` | `Publisher` interface. `Local` (atomic disk write, refuse-first) + `GitLab` (branch + Files API + MR). Frontmatter generation in `frontmatter.go`. |
| `internal/safety` | `Guard` interface. `DecideGuard` wraps `inful/decide` for pre-LLM prompt-injection checks. `NoopGuard` is the default-off path. `FakeGuard` for tests. |
| `internal/run` | The pipeline. `runOneProject` does fetch + normalise + 2× LLM + 2× publish per project. `runRollup` does the cross-project synthesis. Per-MR `fillMRChanges` worker pool. |

## What stays out of scope (today)

- Real-time activity streaming (we poll, not subscribe)
- Operator UI / control plane (everything is config + cron)
- Multi-tenant isolation (one binary, one operator, one vault)
- Webhook-driven runs (we're cron-driven; `--safety-enabled` is the only asynchronous guard)
- Automatic enrollment of new projects (operators curate the subscription list)
