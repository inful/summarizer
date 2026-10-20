# Requirements: summarizer

**Defined:** 2026-10-20
**Core Value:** One command turns a week of GitLab activity into two readable reports, written for the right audience.

## v1 Requirements

Requirements for initial release. Each maps to roadmap phases.

### CFG — Configuration & CLI

- [ ] **CFG-01**: Operator can configure the GitLab URL via YAML config file
- [ ] **CFG-02**: Operator can configure the LLM model choice per project in YAML config
- [ ] **CFG-03**: Operator can configure the output directory in YAML config
- [ ] **CFG-04**: Operator specifies the lookback window via `--since Nd` CLI flag (rolling N days back from now, default 7)
- [ ] **CFG-05**: GitLab token is read from `GITLAB_TOKEN` env var (never from config)
- [ ] **CFG-06**: LLM provider API key is read from a provider-specific env var (e.g. `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`); never from config
- [ ] **CFG-07**: Operator specifies the target project via `--project` CLI flag (path `group/subgroup/project` or numeric ID)
- [ ] **CFG-08**: YAML config is validated against a Zod schema; missing or invalid keys produce a clear error message naming the offending key

### FETCH — GitLab ingest

- [ ] **FETCH-01**: Tool fetches merge requests (opened / merged / closed) within the rolling N-day window
- [ ] **FETCH-02**: Tool fetches issues (opened / closed) within the window
- [ ] **FETCH-03**: Tool fetches commits on the default branch within the window
- [ ] **FETCH-04**: Tool handles GitLab API pagination (Link headers / keyset cursors) without missing or duplicating events
- [ ] **FETCH-05**: Tool respects GitLab rate-limit response headers; backs off and retries on 429
- [ ] **FETCH-06**: Tool authenticates with the configured GitLab instance using `GITLAB_TOKEN` PAT (header `PRIVATE-TOKEN`)
- [ ] **FETCH-07**: Tool works against self-hosted GitLab; host URL is config (default `https://gitlab.com`)
- [ ] **FETCH-08**: Time window is computed in operator's local timezone and is deterministic per invocation (no clock skew between fetch and window calc)
- [ ] **FETCH-09**: Tool normalizes GitLab MR / issue / commit records into a single `NormalizedEvent` shape so prompts do not depend on GitLab JSON

### LLM — LLM provider & prompts

- [ ] **LLM-01**: Tool uses a pluggable LLM provider abstraction (interface, not direct SDK calls); adding a third provider is a one-line factory swap
- [ ] **LLM-02**: Default providers include OpenAI and Anthropic
- [ ] **LLM-03**: Tool calls the LLM once for the customer-facing report
- [ ] **LLM-04**: Tool calls the LLM once for the team-facing report
- [ ] **LLM-05**: Both reports are derived from the same fetched + normalized event set (no re-fetch per audience)
- [ ] **LLM-06**: Customer-facing prompt produces changelog-y output: what shipped, what improved, known issues, written for an external reader
- [ ] **LLM-07**: Team-facing prompt produces engineering-y output: who moved what, blockers, queue, written for the team and their peers
- [ ] **LLM-08**: Tool caps input events at `--max-events N` (default 200); excess is deterministically summarized (counted + sample), and the cap is disclosed in the report
- [ ] **LLM-09**: Prompts treat untrusted GitLab text (MR / issue titles + bodies) as data, not instructions (OWASP LLM01:2025 mitigation)
- [ ] **LLM-10**: Output is validated against a Zod schema before write; malformed LLM output triggers a clear error rather than a half-written file

### OUTPUT — Markdown output

- [ ] **OUT-01**: Tool writes two Markdown files per run: `<period>-customer.md` and `<period>-team.md`
- [ ] **OUT-02**: File writes are atomic (tmp + rename); never leaves a half-written file on crash
- [ ] **OUT-03**: If a target file already exists, tool refuses the write unless `--force` is passed (refuse-first policy)
- [ ] **OUT-04**: Tool writes a run summary to stderr (events fetched, events used after cap, model invoked, file paths written, token usage if available)

### RUN — CLI runtime

- [ ] **RUN-01**: Tool exits 0 on success, non-zero with structured error on failure (distinct codes for config / auth / fetch / LLM / IO)
- [ ] **RUN-02**: Tool supports `--dry-run` mode: fetches GitLab data, prints event count + first/last timestamps + suggested file paths, writes no files, makes no LLM call
- [ ] **RUN-03**: Tool supports `--help` and `--version` flags
- [ ] **RUN-04**: Tool reads no other state from disk (no DB, no log file, no lockfile); each invocation is hermetic
- [ ] **RUN-05**: Compiles to a single static binary via Bun; no runtime required on the host beyond the binary itself

## v2 Requirements

Deferred to future release. Tracked but not in current roadmap.

### Daemon & Scheduling

- **DAEMON-01**: Tool runs as a long-running daemon with internal scheduling
- **DAEMON-02**: Tool supports multiple projects in one invocation
- **DAEMON-03**: Tool exposes a `--cron-template` printer for systemd timer / crontab recipes

### Delivery

- **DELIV-01**: Customer report posted to a Slack channel via webhook
- **DELIV-02**: Team report emailed to configured recipients
- **DELIV-03**: Report published to a GitLab project wiki page
- **DELIV-04**: Report appended to a recurring GitLab MR

### Observability

- **OBS-01**: Per-run metrics written to a local SQLite log (events, token usage, latency)
- **OBS-02**: Run history browsable via a CLI subcommand
- **OBS-03**: Cost guard emits a warning at configurable spend-per-run threshold

### Advanced LLM

- **LLM-11**: Two-stage LLM (summarize-then-summarize) for very-high-activity projects
- **LLM-12**: Per-audience prompt customization via local template files
- **LLM-13**: Local-model provider via Ollama / vLLM

## Out of Scope

| Feature | Reason |
|---------|--------|
| Real-time activity streaming (webhooks, live updates) | Poll/batch model is sufficient for weekly/monthly cadence |
| Interactive chat with the data | Tool is a one-shot generator, not a chat interface |
| Multi-language report generation (English only for v1) | Customer + team are the same locale by default |
| Comment / discussion scraping (MR notes, issue threads) | High prompt-injection surface area; deferred until sanitization patterns are proven on titles + bodies |
| DORA metrics / cycle-time analytics | Different category of product (Swarmia, LinearB territory) |
| Hosted SaaS / multi-tenant dashboard | Internal CLI tool — operator runs on own machine |
| OAuth user flows (login, refresh tokens) | PAT via env is sufficient for self-hosted GitLab |
| Persistent state between runs (databases, caches) | Each invocation is hermetic; speed comes from GitLab's own indexing |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

Each v1 requirement maps to exactly one phase.

| Requirement | Phase | Status |
|-------------|-------|--------|
| CFG-01 | Phase 1 | Pending |
| CFG-02 | Phase 1 | Pending |
| CFG-03 | Phase 1 | Pending |
| CFG-04 | Phase 1 | Pending |
| CFG-05 | Phase 1 | Pending |
| CFG-06 | Phase 1 | Pending |
| CFG-07 | Phase 1 | Pending |
| CFG-08 | Phase 1 | Pending |
| FETCH-01 | Phase 1 | Pending |
| FETCH-02 | Phase 1 | Pending |
| FETCH-03 | Phase 1 | Pending |
| FETCH-04 | Phase 1 | Pending |
| FETCH-05 | Phase 1 | Pending |
| FETCH-06 | Phase 1 | Pending |
| FETCH-07 | Phase 1 | Pending |
| FETCH-08 | Phase 1 | Pending |
| FETCH-09 | Phase 1 | Pending |
| RUN-02 | Phase 1 | Pending |
| LLM-01 | Phase 2 | Pending |
| LLM-02 | Phase 2 | Pending |
| LLM-03 | Phase 2 | Pending |
| LLM-06 | Phase 2 | Pending |
| LLM-08 | Phase 2 | Pending |
| LLM-09 | Phase 2 | Pending |
| LLM-10 | Phase 2 | Pending |
| OUT-02 | Phase 2 | Pending |
| LLM-04 | Phase 3 | Pending |
| LLM-05 | Phase 3 | Pending |
| LLM-07 | Phase 3 | Pending |
| OUT-01 | Phase 3 | Pending |
| OUT-03 | Phase 4 | Pending |
| OUT-04 | Phase 4 | Pending |
| RUN-01 | Phase 4 | Pending |
| RUN-03 | Phase 4 | Pending |
| RUN-04 | Phase 4 | Pending |
| RUN-05 | Phase 4 | Pending |

**Per-phase totals:**

| Phase | Requirements | Count |
|-------|--------------|-------|
| 1. GitLab Ingest | CFG-01..08, FETCH-01..09, RUN-02 | 18 |
| 2. Bounded Single-Audience Report | LLM-01, LLM-02, LLM-03, LLM-06, LLM-08..10, OUT-02 | 8 |
| 3. Dual-Audience Reports | LLM-04, LLM-05, LLM-07, OUT-01 | 4 |
| 4. Unattended-Run Safety | OUT-03, OUT-04, RUN-01, RUN-03..05 | 6 |

**Coverage:**
- v1 requirements: 36 total (CFG 8 + FETCH 9 + LLM 10 + OUT 4 + RUN 5)
- Mapped to phases: 36 — each to exactly one phase, no duplicates
- Unmapped: 0 ✓

*Note: the pre-roadmap draft listed 35 and double-mapped LLM-03..07 to both Phase 2 and Phase 3; both corrected above.*

---
*Requirements defined: 2026-10-20*
*Last updated: 2026-10-20 after roadmap creation (traceability mapped, count corrected 35 → 36)*