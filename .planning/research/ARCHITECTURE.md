# Architecture Research

**Domain:** One-shot CLI report generator (GitLab fetcher → LLM → Markdown)
**Project:** summarizer
**Researched:** 2026-10-20
**Confidence:** HIGH — pattern is well-established; tech-stack-agnostic

## Standard Architecture

This project is a **pipeline**: a fixed sequence of pure-ish stages wired at startup, each stage doing one job and handing off a typed value. There is no event loop, no queue, no daemon in the MVP. The whole thing runs once per invocation and exits.

### System Overview

```
┌──────────────────────────────────────────────────────────────────┐
│                       CLI Boundary (cmd/)                        │
│   summarizer run --project <path> --since 7d --out ./reports     │
└─────────────────────────────┬────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│                      Pipeline (internal/run)                    │
│   fetch → normalize → prompt ×2 → complete ×2 → write           │
└─────┬──────────┬───────────┬─────────────┬────────────┬──────────┘
      │          │           │             │            │
      ▼          ▼           ▼             ▼            ▼
┌──────────┐ ┌─────────┐ ┌──────────┐ ┌─────────┐ ┌─────────────┐
│  Config  │ │ GitLab  │ │ Normal-  │ │   LLM   │ │   Writer    │
│  Loader  │ │ Client  │ │  izer    │ │ Client  │ │             │
│          │ │ (port)  │ │          │ │ (port)  │ │  (port)     │
└──────────┘ └─────────┘ └──────────┘ └─────────┘ └─────────────┘
   adapter    adapter       core        adapter       adapter
```

### Component Responsibilities

| Component | Responsibility | Talks to | Notes |
|-----------|----------------|----------|-------|
| **CLI entry** (`cmd/run.go`) | Parse flags, resolve config, build the pipeline graph, exit with status | Config, Pipeline | Thin. No business logic. |
| **Config loader** (`internal/config`) | Merge flags + config file + env (precedence: flags > env > file > defaults). Per-project overrides. | CLI entry | Single source of truth for host, token path, model, lookback, output dir. |
| **GitLab client** (`internal/gitlab`) | Typed wrapper over GitLab REST v4. Pagination, basic retries, JSON decode. | Config (host + token), Pipeline | One method per resource: `ListMRs`, `ListIssues`, `ListCommits`, `ListMilestones`, `ListNotes`. |
| **Normalizer** (`internal/events`) | Turn heterogeneous GitLab JSON (MR / issue / commit / note / milestone) into one `NormalizedEvent` type. | GitLab client output | The single shape the rest of the pipeline sees. |
| **Prompt builder** (`internal/prompt`) | Render customer + team prompt templates with the event set. Track token budget. | Normalizer output, LLM config | Templates live in `prompts/*.tmpl`, loaded via `embed.FS`. |
| **LLM client** (`internal/llm`) | Pluggable provider. One method: `Complete(ctx, req) (resp, err)`. Retries + cost cap. | Prompt builder, env keys | `OpenAIProvider`, `AnthropicProvider`. Selected by config. |
| **Writer** (`internal/writer`) | Atomic Markdown write (tmp → fsync → rename) to a deterministic path. | Pipeline output | One impl for MVP (`FileWriter`); future: `GitWriter`, `SlackWriter`. |
| **Pipeline** (`internal/run`) | The orchestration function. Calls the above in order. Pure, no I/O of its own. | All of the above | Unit-testable with fake ports. |

The seam between core (`events`, `prompt`, `run`) and adapters (`gitlab`, `llm`, `writer`) is the **port interface**. `events.NormalizedEvent` is the lingua franca — everything to the left of it is "fetch and shape", everything to the right is "render and ship".

## Recommended Project Structure

Two viable layouts. **Pick Go-shaped or Node-shaped based on the stack chosen in `STACK.md`** — both follow the same boundaries.

### Option A: Go layout (recommended for this domain)

```
summarizer/
├── cmd/
│   └── summarizer/
│       └── main.go              # CLI entry, wires the pipeline, exits
├── internal/
│   ├── config/
│   │   ├── config.go            # struct + defaults + merge
│   │   └── config_test.go
│   ├── gitlab/
│   │   ├── client.go            # http.Client wrapper, pagination
│   │   ├── types.go             # raw GitLab JSON types
│   │   └── client_test.go
│   ├── events/
│   │   ├── event.go             # NormalizedEvent type
│   │   ├── normalize.go         # GitLab JSON → NormalizedEvent
│   │   └── normalize_test.go
│   ├── prompt/
│   │   ├── builder.go           # template render + token budget
│   │   ├── prompts/
│   │   │   ├── customer.tmpl
│   │   │   └── team.tmpl
│   │   └── builder_test.go
│   ├── llm/
│   │   ├── provider.go          # Provider interface
│   │   ├── openai.go            # OpenAI adapter
│   │   ├── anthropic.go         # Anthropic adapter
│   │   └── provider_test.go
│   ├── writer/
│   │   ├── writer.go            # Writer interface
│   │   ├── file.go              # FileWriter (atomic)
│   │   └── writer_test.go
│   └── run/
│       ├── pipeline.go          # Run(ctx, cfg) — orchestration only
│       └── pipeline_test.go
├── prompts/                     # empty for Go (templates in internal/prompt)
├── examples/
│   └── config.yaml              # sample per-project config
├── go.mod
├── go.sum
├── README.md
└── .planning/
```

### Structure Rationale

- **`cmd/` and `internal/`**: enforces "this is one binary". Nothing in `internal/` is reusable; that keeps the surface honest.
- **One package per stage**: each stage compiles in isolation; tests don't need the world.
- **`prompts/` separate from `internal/prompt/`**: prompt text is content, edited by humans. Keep code (template vars, render logic) away from prose (audience framing).
- **`internal/run/pipeline.go` is the only place that imports every other package.** All cross-package coupling lives in ~30 lines.
- **`examples/config.yaml`** is a runnable, copy-able sample. Not the only way to configure, just a doc.

## Architectural Patterns

### Pattern 1: Hexagonal (Ports & Adapters)

**What:** Core (`pipeline`, `events`, `prompt`) depends only on small interfaces. Adapters (`gitlab`, `llm`, `writer`) implement those interfaces. Wiring happens once, in `cmd/main.go`.

**When:** Always. Even for MVP. The cost is ~15 lines of interface declarations; the benefit is that the pipeline is testable with fakes and the LLM can be swapped without touching the core.

**Trade-offs:** Slight verbosity (interface + impl). Pays back the first time you write a test.

**Example:**
```go
// internal/llm/provider.go — the port
type Provider interface {
    Complete(ctx context.Context, req Request) (Response, error)
}

// internal/llm/openai.go — the adapter
type OpenAIProvider struct{ Key string; Model string }
func (o *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) { ... }

// internal/run/pipeline.go — the core uses only the port
type Pipeline struct {
    Fetch    GitLabFetcher
    Prompt   PromptBuilder
    Complete llm.Provider
    Write    Writer
}
```

### Pattern 2: Embed Prompt Templates

**What:** Prompt text lives in `.md` files under `internal/prompt/templates/`, loaded via `//go:embed` (`embed.FS`). Rendered at call time with the normalized event set.

**When:** Always. Two audiences = two prompt files. Editing prose in code is the single most common prompt-engineering anti-pattern.

**Trade-offs:** Slightly more setup (template loader). Saves every prompt tweak from becoming a PR review on logic.

**Example:**
```go
//go:embed prompts/*.tmpl
var promptFS embed.FS

func (b *Builder) CustomerPrompt(events []NormalizedEvent) (string, error) {
    return b.render(b.fs, "prompts/customer.tmpl", map[string]any{
        "Events":     events,
        "PeriodDays": b.cfg.LookbackDays,
    })
}
```

### Pattern 3: Fetch Once, Render Twice

**What:** GitLab is fetched and normalized exactly once per invocation. The same `[]NormalizedEvent` is fed into two prompt renders and two LLM calls.

**Why:** It's free parallelism of reasoning (no second fetch needed), and it makes the two reports *comparable* — they describe the same underlying facts.

**Trade-offs:** None for MVP. Costs one slice allocation.

### Pattern 4: Atomic File Writes

**What:** Write to `<name>.tmp`, `fsync`, `rename(<name>.tmp, <name>)`. POSIX rename is atomic.

**When:** Whenever cron / CI invokes the tool. A SIGKILL mid-write must not produce a half-Markdown file the operator forwards.

**Trade-offs:** Trivial; non-negotiable for cron use.

## Data Flow

### End-to-End

```
CLI flags (--project, --since, --out, --model, --config)
        │
        ▼
Config Resolved ──────►  ProjectConfig { host, token_path, model, lookback, output_dir }
        │
        ▼
GitLab Client.FetchAll(ctx, cfg.ProjectPath)
   ├─ GET /projects/:id/merge_requests?updated_after=...
   ├─ GET /projects/:id/issues?updated_after=...
   ├─ GET /projects/:id/repository/commits?since=...
   └─ GET /projects/:id/milestones?updated_after=...
        │
        ▼
[]RawGitLab (heterogeneous JSON)
        │
        ▼
Normalizer.Normalize(raws) → []NormalizedEvent
   each event: { Kind, ID, Title, Body, Author, URL, Timestamp, Refs, Tags }
        │
        ├──► PromptBuilder.CustomerPrompt(events) ──► LLM.Complete ──► customer.md
        │
        └──► PromptBuilder.TeamPrompt(events)     ──► LLM.Complete ──► team.md
```

### Key Data Shapes

```go
// The lingua franca. Everything past the normalizer sees only this.
type NormalizedEvent struct {
    Kind      EventKind     // MERGED_MR | OPENED_MR | CLOSED_ISSUE | COMMIT | MILESTONE | NOTE
    ID        string        // GitLab global ID or commit SHA
    Title     string        // MR title, issue title, commit subject
    Body      string        // MR description, issue body, commit message body
    Author    string        // display name or username
    Timestamp time.Time     // normalized to UTC
    URL       string        // canonical web URL
    Refs      []string      // issue refs, MR refs, label names
    Tags      []string      // audience-agnostic facets: "frontend", "auth", "breaking"
}

// The LLM request — everything downstream of the prompt builder is text in / text out.
type llm.Request struct {
    System   string
    Prompt   string
    Model    string
    MaxOut   int
}

// The file result — what gets written.
type writer.Result struct {
    Audience string         // "customer" | "team"
    Path     string         // ./reports/2026-W43-customer.md
    Markdown string
    TokensIn int
    TokensOut int
}
```

### State Management

None. The pipeline is **stateless between stages**. Each stage takes inputs, returns outputs. No globals, no singletons, no caches across runs. This is what makes the tool safe to invoke from cron.

## Scaling Considerations

This tool scales **per run**, not per user. Real limits:

| Scale (per project, per window) | Behavior |
|---------------------------------|----------|
| 0–50 events | No action needed. One prompt, one LLM call. |
| 50–500 events | Token budget guard kicks in: aggregate low-signal events (commit list, minor issues) into a "Other activity" block before the LLM sees them. |
| 500–5000 events | Two-stage LLM: first call summarizes batches of ~200 events into "section summaries"; second call stitches section summaries into the final report. **This is the deferred cost-control work flagged in PROJECT.md.** |
| 5000+ events | Probably a project that should be reported on monthly, not weekly. Cap by truncating with a note in the report ("+ 1,247 additional commits not shown"). |

### Scaling Priorities

1. **First bottleneck:** LLM token cost. Solve via budget guard + aggregation before adding any infra.
2. **Second bottleneck:** GitLab API rate limit on a busy project. Solve via per-resource `updated_after` filters and conditional GET (ETag / Last-Modified) — **only after cost is handled**, since rate limit is rare on self-hosted.

Multi-project runs (deferred) would need a queue and worker pool. Not for MVP.

## Anti-Patterns

### Anti-Pattern 1: Spreading GitLab JSON across the codebase

**What people do:** Let the GitLab response struct leak into prompt templates or the writer.

**Why it's wrong:** Tying the LLM prompt to GitLab's response shape means every GitLab API change becomes a prompt review. It also makes the writer format GitLab-internal fields (`iid`, `project_id`) into the customer report.

**Do this instead:** Normalize once at the boundary. `NormalizedEvent` is the only type the prompt and writer ever see.

### Anti-Pattern 2: LLM-framework sprawl

**What people do:** Wrap the LLM call in a "chain" / "agent" abstraction with retry policies, JSON schema parsing, tool calls, conversation memory.

**Why it's wrong:** This is one-shot text-in / text-out, twice per run. ~80 lines of plain HTTP is enough. Frameworks add indirection without earning it.

**Do this instead:** One `Provider` interface, one `Complete(ctx, req)` method, one HTTP call per audience. Retry with `cenkalti/backoff` or stdlib. Done.

### Anti-Pattern 3: Inline prompt strings

**What people do:** Build the prompt with `fmt.Sprintf("You are a %s. Here are events: %s", ...)` inside `pipeline.go`.

**Why it's wrong:** Prompts are content, not code. Editing tone or structure means re-running tests for unrelated logic. Code review can't see prompt diffs clearly.

**Do this instead:** `.tmpl` files in `internal/prompt/prompts/`, embedded, version-controlled separately from logic.

### Anti-Pattern 4: Fetching twice

**What people do:** Call GitLab once for the customer prompt, once again for the team prompt (or for "freshness").

**Why it's wrong:** Two fetches = two network windows, two pagination passes, two chances for inconsistency between the two reports. Also slower for no reason.

**Do this instead:** Fetch once, normalize once, hand the same `[]NormalizedEvent` to both audiences. The two reports describe the same facts — that's the whole point.

### Anti-Pattern 5: Non-atomic Markdown write

**What people do:** `os.WriteFile(path, body, 0644)`.

**Why it's wrong:** A SIGKILL during the write leaves a half-file the operator may forward. Cron + a flaky disk = corrupted reports.

**Do this instead:** Write to `path.tmp`, `f.Sync()`, `os.Rename(tmp, path)`. ~10 lines.

## Integration Points

### External Services

| Service | Pattern | Notes |
|---------|---------|-------|
| **GitLab REST v4** | Plain `net/http` (stdlib) with PAT in `PRIVATE-TOKEN` header. Pagination via `Link: rel="next"` or `?page=` + `X-Total-Pages`. | Self-hosted: `host` is config. PAT comes from env var `GITLAB_TOKEN`; the tool reads but never persists. |
| **OpenAI / Anthropic** | Provider interface. Default selected in config. API key from `OPENAI_API_KEY` / `ANTHROPIC_API_KEY`. | Both expose a simple chat-completion style call. Token budget enforced client-side (estimate before sending). |
| **Filesystem** | `FileWriter` is the only writer in MVP. Path = `<output_dir>/<YYYY-Wnn>-<audience>.md`. | Configurable; defaults to `./reports`. |

### Internal Boundaries

| Boundary | Communication | Notes |
|----------|---------------|-------|
| `cmd/main.go` → `run.Pipeline` | Constructor injection | All wiring lives here. The pipeline itself never imports `gitlab`, `llm`, or `writer` concretely — only their interfaces. |
| `run.Pipeline` → `events.NormalizedEvent` | Typed slice passed by value | One struct, one shape. No maps, no `any`. |
| `prompt.Builder` → `llm.Provider` | One `Complete()` call per audience | No streaming, no tokens-out callback, no tool use in MVP. |
| `writer.Writer` → filesystem | `Write(name, markdown)` returns path | Returns the path it actually wrote (after rename) so the pipeline can log it. |

## Build Order

Dependency graph dictates order. Arrows mean "depends on":

```
config ──► gitlab ──► events ──► prompt ──► llm ──► writer ──► run ──► cmd
                                                  │
                                                  └──► (parallel: cmd can stub writer at first)
```

### Recommended Phase Structure (4 phases, matches `granularity: coarse`)

**Phase 1 — Foundation:** scaffold, config loader, CLI skeleton, logger, `summarizer run --help` works.
- Files: `cmd/summarizer/main.go`, `internal/config/config.go`, `internal/config/config_test.go`.
- Output: `summarizer run --project foo --since 7d` exits cleanly with "no GitLab data" placeholder.

**Phase 2 — GitLab fetch + normalize (no LLM):** prove the fetch/normalize shape against real GitLab.
- Files: `internal/gitlab/`, `internal/events/`, plus `--dry-run` flag that prints normalized events as JSON to stdout.
- Output: operator can eyeball the normalized event shape before any LLM cost.

**Phase 3 — LLM + one audience + write:** end-to-end loop with the customer prompt only.
- Files: `internal/llm/`, `internal/prompt/` (customer template only), `internal/writer/`, `internal/run/pipeline.go` wired up but calls LLM once.
- Output: `summarizer run --project foo` produces `<out>/<period>-customer.md`.

**Phase 4 — Second audience + polish + cost guard:** team prompt, multi-event types (issues, commits, milestones), token-budget guard, atomic-write confirmation, `--dry-run` and `--verbose` flags, sample config.
- Files: `internal/prompt/prompts/team.tmpl`, extension of normalizer to all event kinds, budget guard in `internal/llm/`.
- Output: MVP complete. Two files per run, on a real GitLab project.

### Why this order

- **Config first** because every other component needs settings.
- **GitLab before LLM** because the LLM prompt is uninteresting until you see what data lands in it. The dry-run step (Phase 2) is the cheapest place to catch schema drift.
- **One audience before two** because "is this report any good?" can be answered with one prompt. Two prompts without the first one good is double the prompt-engineering thrash.
- **Polish last** so the cost guard lands after you've seen real token counts from one prompt working.

### Alternative: 3-phase compression

If 4 phases feels like ceremony, collapse Phases 1+2 into "Foundation + fetch" and Phases 3+4 into "LLM + ship". The dependency is the same; you lose the dry-run checkpoint.

## Sources

Domain knowledge — well-trodden pattern, no single canonical reference. Comparable shapes:

- **Changelog generators** (e.g. `git-cliff`, `release-please`, `towncrier`): config + multi-source fetch + template render + file write. Identical pipeline minus the LLM stage.
- **AI digest CLIs** (e.g. `ai-digest`, `hn-summary`, podcast-from-rss): RSS/API fetch → normalize → LLM summarize → file output. Same pattern, swap GitLab for RSS.
- **Status reporters** (`jira-status-reporter`, `standup-bot`): ticket fetch → LLM compose → chat/file output. Same pattern, swap GitLab for Jira.
- **GitLab REST v4 docs** (api.gitlab.com): confirmed resource paths (`/projects/:id/merge_requests`, `/issues`, `/repository/commits`, `/milestones`); pagination via `?page=` + `X-Next-Page`; auth via `PRIVATE-TOKEN` header.
- **OpenAI / Anthropic API docs**: both expose a single chat-completion call sufficient for this tool's needs.

---

*Architecture research for: summarizer (GitLab project summarizer)*
*Researched: 2026-10-20*