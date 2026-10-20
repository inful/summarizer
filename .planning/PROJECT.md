# summarizer (GitLab project summarizer)

## What This Is

A CLI which, given one GitLab project, fetches its activity over the last N days and produces two Markdown reports — one for the project's **customers** (changelog-y: what shipped, what's better, what's known broken) and one for the **team and their peers/colleagues** (engineering-y: who moved what, where the blockers are, what's queued). The LLM frames the same GitLab events differently for each audience. MVP is a one-shot CLI; promote to a long-running daemon later if the shape proves out.

## Core Value

One command turns a week of GitLab activity into two readable reports, written for the right audience.

## Business Context

<!-- Internal tool — kept lean on purpose. -->

- **Customer**: the operator themselves + their team and project customers (small internal audience)
- **Revenue model**: none — internal CLI tool
- **Success metric**: "I run this Monday morning and the team + customer reports are good enough to forward with zero or trivial edits"
- **Strategy notes**: N/A

## Requirements

### Validated

<!-- Shipped and confirmed valuable. -->

(None yet — ship to validate)

### Active

<!-- Current scope. Building toward these. -->

- [ ] Fetch GitLab MR / branch / issue / commit activity for one project over a rolling N-day window
- [ ] Generate a **customer-facing** Markdown report (changelog-y) via LLM
- [ ] Generate a **team-facing** Markdown report (engineering-y) via LLM
- [ ] Configurable per project: GitLab URL, lookback window, output directory, LLM model choice
- [ ] Pluggable LLM provider; default OpenAI / Anthropic
- [ ] Authenticate GitLab via Personal Access Token from env (`GITLAB_TOKEN`)
- [ ] One-shot CLI invocation: `summarizer run --project <path> --since 7d`
- [ ] Two Markdown files written per run: `<period>-customer.md` and `<period>-team.md`

### Out of Scope

- Built-in scheduler / cron-like daemon mode — start as one-shot CLI; promote to long-running only after MVP proves out *(deferred — see Decisions)*
- Multi-project runs in one invocation — CLI is one project at a time *(deferred)*
- Slack / email / wiki / GitLab wiki delivery — MVP writes Markdown only *(deferred)*
- Hosting / SaaS / multi-tenant dashboard — internal CLI tool, runs on operator's machine *(explicit exclusion)*
- Per-user OAuth flows — PAT via env is sufficient for self-hosted GitLab *(explicit exclusion)*

## Context

- Working dir: `/Users/inful/Code/github.com/inful/summarizer` — greenfield, no existing code.
- GitLab: self-hosted, REST API v4. Host URL is config.
- LLM: pluggable provider, default OpenAI or Anthropic. Per-project model choice in config.
- Time window: rolling N days back from now (e.g. `--since 7d`). Cron at a fixed time always covers the same span.
- Operator's existing setup includes Open Second Brain, Flux, tokensave, etc. — none of those are consumed by this tool, just context.
- Open thread flagged for planning phase: **cost control on the LLM call**. If a project has hundreds of MRs in a window, we need a strategy (cap events / sample / summarize-in-stages / truncate with a note). Not blocking MVP scope, but must be addressed before the tool is run on a real high-volume project.

## Constraints

- **Compatibility**: must run on operator's macOS machine (per working dir + existing toolchain).
- **Tech stack**: **Go (1.23+)** — operator-specified. Stdlib for HTTP, flags, FS, env, tests. Single third-party dep: `gopkg.in/yaml.v3`. Compiles to a single static binary via `CGO_ENABLED=0 go build`.
- **Dependencies**: GitLab REST API v4 + at least one LLM provider API (OpenAI or Anthropic). Both are plain HTTP; no SDKs.
- **Auth**: GitLab token from env var `GITLAB_TOKEN`. LLM API key from env (provider-specific). No tokens stored in config files.
- **Output**: Markdown only for MVP. No rich HTML, no PDF, no Slack formatting.

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| One-shot CLI for MVP | Simplest, most debuggable; defers scheduler/PID/state complexity | — Pending |
| Two reports per run (customer + team) | Same GitLab events, different audience framings — distinct value from one fetch | — Pending |
| Markdown files only (MVP) | Easy to version, easy to inspect, no Slack/wiki plumbing to fail | — Pending |
| Pluggable LLM, default OpenAI/Anthropic | Avoid early lock-in; one provider is fine for MVP | — Pending |
| Rolling N-day window (not calendar week) | Freshest activity; cron at fixed time always covers the same span | — Pending |
| One project per invocation | Simplest CLI shape; multi-project deferred | — Pending |
| Self-hosted GitLab + PAT from env | Operator runs on own machine; PAT via env is the standard self-hosted pattern | — Pending |
| Sequential execution (over parallel) | Small project — parallel adds merge friction without wall-clock payoff | — Pending |
| Coarse granularity (3–5 phases) | Small MVP, vertical slices preferred | — Pending |
| YOLO mode (auto-approve gates) | Goal is well-defined; user wants to keep moving | — Pending |
| **Go (1.23+)** as the language | Operator-specified after research drafted Bun + TypeScript. Stdlib covers HTTP, flags, FS, env, tests; one third-party dep (`gopkg.in/yaml.v3`); single static binary via `go build` | — Pending |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition** (via `/gsd-transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd-complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-10-20 after initialization*