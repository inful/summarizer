# Feature Research: summarizer (GitLab project weekly summarizer)

**Domain:** Developer tool — CLI that turns a rolling N-day window of one GitLab project's activity into two LLM-written Markdown reports (customer-facing + team-facing).
**Researched:** 2026-10-20
**Confidence:** MEDIUM
**Mode:** Ecosystem (open-source + commercial analogues)

> Scope of this document: the *features* of the category. Stack details live in STACK.md, architecture in ARCHITECTURE.md, pitfalls in PITFALLS.md, and the project's own decisions are in `.planning/PROJECT.md` (already validated against).

---

## Executive Summary

The category — "fetch git activity for a window, write a human summary" — has at least four mature branches:

1. **Open-source GitHub/GitLab digest CLIs** (`bzz/weekly-digest-cli`, `abensur/github-activity-digest`, `sahansera/gh-weekly-updates`, `yinzhenyu-su/weekly-git-summary`, `yorch/cc-analyzer`). They converge on `--since/--until`, pluggable LLM provider, Markdown output, PAT auth, and optional cache/retry. None of them produce *two* audiences from *one* fetch.
2. **Voice-style changelog generators** (`evoleinik/changelog-summary` shipping "developer" / "founder" / "marketing" voice styles). This is the closest structural analog to the dual-audience idea and the strongest evidence that the framing maps cleanly to one LLM prompt per file.
3. **Customer-facing release-tools SaaS** (LaunchNotes, Beamer, Headway, ReleaseNotes.io) — strong on customer delivery, weak on internal engineering transparency; require hosted infrastructure the operator does not need.
4. **Engineering intelligence analytics** (Swarmia, LinearB, Jellyfish, Waydev, Pluralsight Flow, Faros AI) — DORA/cycle-time metrics, dashboards, not narrative digests. Out of band for this tool.

The gap our `summarizer` fills: **one self-hosted GitLab project + one rolling window → two audiencially distinct `.md` files, no SaaS, no scheduler, no payload delivery.** That gap is the core differentiator — most competitors pick one slice (customer release notes OR engineering standup OR metrics) and stand on it.

---

## Table Stakes

Features users expect from a tool in this category. Missing these and the tool feels broken or unfinished.

| Feature | Why expected | Complexity | Notes |
|---|---|---|---|
| **CLI invocation, one command per run** | Every comparable tool (`weekly-digest-cli`, `gh-weekly-updates`, `git-summarizer`, `abensur/github-activity-digest`) is invoked once with flags; failure mode for "tool that needs setup to be useful" is high. | LOW | One binary/entrypoint, `summarizer run --project <path> --since 7d`. No TTY loops, no wizard. |
| **Auth via env-var Personal Access Token** | PAT-from-env is the standard self-hosted GitLab pattern (also GitHub's standard). Storing tokens in config or pushing OAuth flows is the wrong default. | LOW | Read `GITLAB_TOKEN` (and `OPENAI_API_KEY` / `ANTHROPIC_API_KEY`) on each run; reject empty with a clear error. |
| **Configurable rolling lookback window** | Every inspected CLI exposes `--since` / `--until` or `days=N`. Hardcoding "last week" forces users to fork the tool. | LOW | Accept a duration (`--since 7d`) or explicit ISO date (`--since 2026-10-13`). Defaults to `7d`. |
| **GitLab REST API v4 data fetch** | Merge requests, issues, commits, and notes are the canonical activity surface (`/merge_requests`, `/issues`, `/projects/:id/repository/commits`, all with `updated_after` / `created_after`). Self-hosted means custom `GITLAB_URL`. | MEDIUM | One GitLab project per run. Pagination via `Link` header. Per-endpoint since-filter. Honour `Retry-After` on 429. |
| **Pluggable LLM provider with multiple adapters** | All modern digest CLIs ship Anthropic + OpenAI at minimum; `evoleinik/changelog-summary` also adds Gemini. Locking to one vendor on day one is a regret. | MEDIUM | Provider chosen via config (`llm.provider`) or `--provider`. Default OpenAI; Anthropic supported. |
| **Per-project model choice in config** | Cost-quality tradeoff differs per project (`claude-sonnet` vs `gpt-4o-mini`). Hardcoding the model is brittle. | LOW | Single YAML/TOML config file (e.g. `summarizer.yaml`) with `gitlab`, `llm`, `output` blocks. |
| **Markdown output files written per run** | All inspected OSS tools emit `.md`. Markdown is version-controllable, inspectable, and survives no SaaS pivots. | LOW | Two files: `<period>-customer.md`, `<period>-team.md`, written under `--out <dir>` (defaults to `./summarizer-out/`). |
| **Per-file audience framing via prompt** | `evoleinik/changelog-summary` proves the "different voice per file from same data" pattern. Single generic prompt → single generic file is half a tool. | MEDIUM | Two prompt templates shipped with the binary (`prompts/customer.md.tmpl`, `prompts/team.md.tmpl`); both consume a normalised per-project event stream. |
| **Structured error output and exit codes** | Existing tools exit 0/non-zero with stderr message. CI consumers (cron on macOS, future GitLab CI) need machine-readable status. | LOW | Exit 0 = clean run, 2 = GitLab fetch failure, 3 = LLM API failure, 4 = output write failure, 5 = config invalid. One-line JSON on `--json-errors`. |
| **Reasonable defaults, zero-config run** | `summarizer.yaml` defaults; flag overrides. "Just works on a clean clone with two env vars set" wins against "RTFM first." | LOW | Sensible defaults: `lookback=7d`, `output=./summarizer-out`, `provider=openai`, `model=gpt-4o-mini`. |
| **Token / rate-limit awareness on GitLab calls** | GitLab self-hosted API enforces `User and IP Rate Limits`; GitLab.com updated their public rate limits in Oct 2026. Naive pagination = throttling. | MEDIUM | Honour `Retry-After`, paginate with `per_page=100` (max), expose `--max-events N` to cap. |
| **Cost control on the LLM call** | **Project-open thread.** A high-volume MR project can blow token budget in one call. Caps / sampling / summarise-in-stages is unavoidable before production use. | MEDIUM | Per-run `--max-events N` cap, with explicit "N+ events seen; oldest dropped" note in the output. Defer staged summarisation to v1.x; flag in docs that v1 is *truncate*. |

---

## Differentiators

Features that set `summarizer` apart from every comparable tool I inspected. Each is rooted in the project's *Core Value*: "one command turns a week of GitLab activity into two readable reports, written for the right audience."

| Feature | Value proposition | Complexity | Notes |
|---|---|---|---|
| **Two audiences, one fetch (the core differentiator)** | No inspected tool produces customer-facing *and* team-facing Markdown from a single window. SmartNote (arxiv 2505.17977) confirms the audience-framing idea has research merit; `evoleinik/changelog-summary` ships multiple voice styles from same data — but for one file, one audience at a time. | MEDIUM (prompt design) | Single normalised event stream → two parallel LLM calls with two distinct prompts. *This is the feature.* Everything else exists to make it cheap and reliable. |
| **Self-hosted GitLab as a first-class target** | Most OSS CLIs default to `github.com`; commercial tools rarely support self-hosted without SSO gymnastics. The operator runs a self-hosted instance — PAT-from-env is the right shape. | LOW | Avoid OAuth, project-access-token flows, instance admin calls. Treat the instance as a plain REST API. |
| **PAT-only auth, by design** | No OAuth, no GitLab App registration, no JWT. This is *deliberately* minimum-credential: the operator's own machine, the operator's own PAT. | LOW | Document that the PAT needs `read_api` scope (or equivalent). Reject non-`glpat-` tokens with a clear note. |
| **Two-file MVP output, no delivery plumbing** | The forward-the-Markdown-to-Slack step is a separate decision. Shipping Markdown files decouples "tool that summarises" from "tool that delivers." | LOW | Output is just `.md`. Future v1.x can add `--post-to <hook>` later if wanted; do not pre-build the hook surface. |
| **Rolling window, not calendar week** | A cron at fixed time always covers the same N-day span; calendar weeks jump. `evoleinik/changelog-summary` uses "Daily Update"; our tool uses rolling window by default. | LOW | Single `--since` flag; `--until` optional (defaults to now). |
| **Per-project config kept tiny** | The competitor norm (LaunchNotes, GitHub auto release notes) drifts toward opinionated metadata. Our config stays to: GitLab URL, output dir, lookback default, provider, model. ~6 keys. | LOW | TOML or YAML; validate with a schema; print a diff on config change. |
| **Operates as a normal Unix CLI** | `summarizer run …` exits; stdout reserved for status, stderr for warnings, files for the actual reports. Plays well with cron, Notion imports, `>>log`, Git checked-in artifacts. | LOW | No TUI, no `browse`, no `serve`. |

---

## Anti-features (deliberately not building)

Features that look like good ideas, that several comparable tools *have*, and that would actively damage this project. Each is a "no" with a reason and a better alternative.

| Anti-feature | Why tempting | Why we're skipping | What to do instead |
|---|---|---|---|
| **Built-in scheduler / daemon mode** | Several analogue tools (e.g. `abensur/github-activity-digest` runs as a scheduled GitHub Action; kestra workflows) bake cron in. | The project decided one-shot MVP; the operator already uses macOS launchd / cron. A daemon adds PID, state, restart, log-rotation, and debugging surface that gains nothing for a "Monday morning" tool. | Document a one-liner: `0 9 * * 1 cd /path/to/summarizer && ./summarizer run --project foo/bar`. Promote to a long-running mode *only* if a real use case appears. |
| **Multi-project in one invocation** | Engineering analytics platforms (Swarmia, LinearB) span many repos; LaunchNotes handles release-on-multi-repo. | MVP is one project per run, by design (PROJECT.md, "Out of Scope"). Cross-project merging requires normalisation of labels, contributors, and timezones, none of which is cheap. | `summarizer run --project a/b`; user shells. If a use case for "weekly across a group" emerges, build `--project-list file` in v2. |
| **Slack / email / GitLab wiki / Notion delivery** | LaunchNotes and Beamer live here; AI summary Actions auto-post to Slack. | Each target has its own auth, format, rate-limit, and failure modes. The reports are the *deliverable*; transporting them is the consumer's call. Output two `.md` files; let the operator paste them wherever they want. | The user copies the Markdown into Slack/email/Notion. If a *specific* target is requested later (e.g. "auto-post to Slack once a week"), add the single `--post-to slack` flag in v1.x. |
| **Per-user OAuth / GitLab App registration** | Web apps want this; LaunchNotes SaaS uses OAuth. | Adds token rotation, refresh, scope-management, GitLab Admin setup. The operator runs the tool on their own machine with their own PAT — OAuth is the wrong shape. | Single env var: `GITLAB_TOKEN`. Reject OAuth responses with a clear "tool is PAT-only" message. |
| **Cross-run persistence (database, history, caching)** | `abensur/github-activity-digest` ships a 30-min file cache; analytics tools persist everything. | Stale output is worse than recomputed output for a once-a-week tool. A cache layer that lies is a worse failure mode than a 30-second re-fetch. SQLite/JSON history invites "summariser of all my summaries" features that drift scope. | Recompute every run. If GitLab pagination becomes the bottleneck, add an in-memory request cache (not on disk) keyed by URL + since-date, expire at process exit. |
| **DORA metrics, cycle-time, throughput dashboards** | Swarmia / LinearB / Jellyfish / Waydev all do this. Looks adjacent to "team-facing report." | These are platform-shaped products with months of metric definitions, baselines, and team-comparison logic. Adding any of them is the start of a 6-month product, not a feature. | The team-facing report stays *narrative* ("who moved what, where the blockers are"). If the operator later wants numeric metrics, that is a separate tool. |
| **HTML / PDF / branded-rendered outputs** | Beamer / Headway produce branded pages. "Pretty" output is a SaaS selling point. | The Markdown *is* the deliverable. Render-once-per-week-then-forward; a renderer step makes the tool harder to debug and inspect. | Operator pastes the `.md` into the destination's renderer (Notion / Slack / email client) on send. |
| **Real-time webhooks / push-mode ingestion** | GitLab supports webhooks; "live" always sounds good. | Requires a public endpoint, webhook secret, retry queue, signature verification, idempotency. Adds server-shaped complexity to a CLI-shaped tool. | Re-run on cron. Output is always stale-by-rolling-window, which is the point. |
| **Audience template editor / template DSL** | Competitors ship "release-note templates" as YAML/TOML knobs. | A template engine is a config surface customers will misconfigure, producing silently-broken reports. | Two prompts ship with the binary; v1.x may add `--prompts-dir` to override. |
| **Multi-language output translation** | `abensur/github-activity-digest` outputs `language: Portuguese` etc. | Adds LLM cost and requires verifying the LLM's grasp of the secondary language. Our operator writes English; defer until the operator or a customer actually asks. | Default English; v2+ can add `--language` over the same pluggable LLM provider. |

---

## Feature dependencies

The minimum graph from MVP scope to differentiators. Anything in this graph that isn't already shipped is the MVP build.

```
[CLI entrypoint (run subcommand)]        ── table-stakes, foundation
    │
    ├──> [Config file loader + env-var override]      ── table-stakes
    │
    ├──> [GitLab REST v4 client]
    │         │
    │         ├──> [MR fetch (since N days, pagination, rate-limit aware)]    ── table-stakes
    │         ├──> [Issue fetch (since N days, includes label/state)]        ── table-stakes
    │         ├──> [Commit fetch (since N days, with author info)]            ── table-stakes
    │         │
    │         └──> [Normalised event-stream model]     ── internal helper
    │                    │
    │                    ├──> [LLM provider adapter (OpenAI / Anthropic)]    ── table-stakes
    │                    │         │
    │                    │         ├──> [Customer-facing prompt + render]   ── ★ differentiator
    │                    │         └──> [Team-facing prompt + render]       ── ★ differentiator
    │                    │
    │                    └──> [Token / event-count guard]                    ── table-stakes (project-open thread)
    │
    ├──> [Markdown writer (one file per audience)]    ── table-stakes
    │
    └──> [Structured error output + exit codes]       ── table-stakes
```

**Conflicting / non-dependencies** (informative — these should NOT be in the same phase):
- *Scheduler mode* ⨯ *CLI-only MVP*: scheduled-mode needs long-running process state; one-shot doesn't. Ship one-shot first; only reconsider if a real use case appears.
- *Cross-run persistence* ⨯ *No-stale-output promise*: a cache that lies is worse than no cache. Default to no cache in v1.
- *Multi-project run* ⨯ *One-project-per-invocation* (the latter is the v1 contract).

---

## MVP definition

### Launch With (v1, the project's "Active" requirements)

These are the items validated against `.planning/PROJECT.md` "Active" section.

- [x] CLI invocation `summarizer run --project <path> --since 7d` *(low complexity, foundation)*
- [x] PAT auth from `GITLAB_TOKEN` *(low, security boundary)*
- [x] Per-project config: `gitlab.url`, `lookback`, `output.dir`, `llm.{provider,model}` *(low)*
- [x] Pluggable LLM provider — at minimum OpenAI and Anthropic *(medium)*
- [x] Fetch GitLab MR + issue + commit activity for one project over rolling N-day window *(medium — API shape, rate limits, normalise)*
- [x] Generate **customer-facing** Markdown via LLM, framed for "what shipped / what's better / what's known broken" *(medium — prompt design)*
- [x] Generate **team-facing** Markdown via LLM, framed for "who moved what / blockers / queued" *(medium — prompt design)*
- [x] Write two Markdown files per run: `<period>-customer.md`, `<period>-team.md` *(low)*
- [x] Structured error output with non-zero exit codes for GitLab/LLM/write failures *(low)*
- [x] Cap on events fed to LLM (`--max-events N`) + warning note when truncated *(medium — required because of the project-open cost thread)*

### Add After Validation (v1.x)

- [ ] `--post-to slack` (single webhook target — if there's a real call for it) *(medium)*
- [ ] Custom prompts via `--prompts-dir` *(low, low priority)*
- [ ] In-memory request cache to reduce GitLab API pressure on large projects *(low)*
- [ ] Additional LLM providers (Gemini, local Ollama) *(low per provider)*
- [ ] GitLab **issues** `epic` linkage grouping in the team report *(low)*

### Future Consideration (v2+, explicitly excluded in PROJECT.md)

- [ ] Built-in scheduler / daemon mode *(trigger: a real weekly-routine use case that cron can't satisfy)*
- [ ] Multi-project in one invocation *(trigger: group/team-level weekly reports become a recurring ask)*
- [ ] Slack / email / Notion / wiki delivery *(trigger: a *specific* target the operator keeps running manually)*
- [ ] Hosting / SaaS / multi-tenant dashboard *(explicit exclusion: internal CLI tool only)*
- [ ] Per-user OAuth flows *(explicit exclusion: PAT is sufficient for self-hosted)*
- [ ] DORA / cycle-time / throughput metrics *(out of scope — separate tool territory)*
- [ ] HTML / PDF / branded outputs *(out of scope — Markdown-only MVP)*

---

## Feature prioritisation matrix

| Feature | User value | Implementation cost | Priority |
|---|---|---|---|
| One command, one project, two Markdown files | HIGH | MEDIUM (the two-prompt framing is the design cost) | **P1** |
| Rolling `--since N` window | HIGH | LOW | **P1** |
| PAT auth from env | HIGH (security) | LOW | **P1** |
| GitLab MR + issue + commit fetch | HIGH | MEDIUM (API, pagination, rate limits) | **P1** |
| Pluggable LLM (OpenAI + Anthropic) | HIGH | MEDIUM (provider abstraction) | **P1** |
| Per-project config file | MEDIUM | LOW | **P1** |
| Event cap + truncation note | MEDIUM (cost) | LOW | **P1** |
| Structured exit codes / error output | MEDIUM | LOW | **P1** |
| Two audience prompts, one per file | **HIGH (the differentiator)** | MEDIUM | **P1** |
| `--max-events` cap | MEDIUM | LOW | **P2** (only because the design constraint exists, but lower-stake if MVP just truncates) |
| `--post-to <slack>` | LOW | MEDIUM (target surface) | P3 |
| Custom prompts dir | LOW | LOW | P3 |
| In-memory GitLab fetch cache | LOW | LOW | P3 |
| Additional LLM providers (Gemini, Ollama) | LOW | LOW per provider | P3 |
| Scheduler / daemon mode | LOW | HIGH (state, restart, logs) | P3 / probably never |
| Multi-project in one invocation | LOW–MEDIUM | MEDIUM (cross-project normalisation) | P3 |
| OAuth flows | LOW (anti-need) | HIGH | **explicitly excluded** |
| DORA / cycle metrics | LOW (different product) | HIGH | **out of scope** |

Priority key:
- P1 — must have for v1 launch.
- P2 — should have, add when the v1 saturates.
- P3 — nice to have, post-validation.

---

## Competitor feature analysis

How the directly comparable tools stand on the rows that matter for *this* project. The two left columns are the closest open-source analogues; the right columns are the relevant commercial categories.

| Feature | `bzz/weekly-digest-cli` (OSS, GitHub) | `abensur/github-activity-digest` (OSS, GitHub) | `sahansera/gh-weekly-updates` (OSS, GitHub) | `evoleinik/changelog-summary` (OSS Action) | **Our summarizer** | LaunchNotes / Beamer (SaaS, customer-facing) | Swarmia / LinearB / Jellyfish / Waydev (analytics) |
|---|---|---|---|---|---|---|---|
| Per-run CLI | ✅ | ✅ | ✅ | ⚙ (GitHub Action) | **✅** | — | — |
| Self-hosted GitLab target | ❌ (GitHub) | ❌ (GitHub) | ❌ (GitHub) | ❌ (GitHub commits only) | **✅** | ⚙ (varying) | ⚙ (varying) |
| PAT from env | ✅ | ✅ | ✅ | ✅ | **✅** | — | — |
| Pluggable LLM | ⚙ (you fork the script) | ✅ (Anthropic, OpenAI) | ✅ (any OpenRouter) | ✅ (Gemini, OpenAI, Anthropic) | **✅** | Hosted only | N/A |
| Rolling lookback window | ❌ (one-week hardcoded) | ✅ (days) | ✅ (--since/--until) | n/a | **✅** | n/a | n/a |
| Two audiencially distinct files from one fetch | ❌ | ❌ | ❌ (one standup voice) | ⚙ (one voice at a time, chosen by flag) | **✅** | ✅ (audience-segmented delivery) | n/a |
| Markdown to local dir | ✅ | ✅ | ⚙ (pushes to repo) | ✅ | **✅** | ❌ (SaaS-rendered) | ❌ |
| No scheduler / no SaaS / no DB | ✅ | ✅ | ✅ | ✅ | **✅** | ❌ (scheduled publishing) | ❌ |
| Per-project config | ⚙ (CLI args only) | ✅ (config.json) | ✅ (config.yaml) | ⚙ (action inputs) | **✅** | ✅ | ✅ |
| Multi-repo / multi-project | ❌ | ✅ (org / user / topics / list) | ✅ (--repos / auto-discover) | ❌ | **❌ (deferred)** | ✅ | ✅ |
| Slack / email delivery | ❌ | ⚙ (Slack/Discord webhook) | ⚙ (push to repo) | ✅ (Slack) | **❌ (deferred)** | ✅ | ⚙ |
| Engineering DORA metrics | ❌ | ❌ | ❌ | ❌ | **❌ (explicit)** | ❌ | ✅ |
| Audience template engine | ❌ | ⚙ (prompt template file) | ✅ (prompt_file) | ✅ (voice styles) | **⚙ (two built-in prompts in v1; --prompts-dir deferred)** | ✅ | n/a |

**Takeaway**: the column for `summarizer` looks sparse by SaaS-analytics standards, and that is exactly the point. Tools like Swarmia and LaunchNotes are software products; this tool is a CLI you run once on Monday morning. The headline differentiator — *one fetch → two audiencially distinct Markdown files for self-hosted GitLab, no SaaS, no scheduler* — is the row nobody else in the table fills.

---

## Sources

**Direct competitor inspection (fetched & read in full):**
- `bzz/weekly-digest-cli` (GitHub) — github.com/bzz/weekly-digest-cli — Node/TypeScript CLI, GitHub-focused, GSoC-inspired; TODOs reveal the standard CLI gaps (--to/--from args, prompt templates, filters).
- `abensur/github-activity-digest` (GitHub Action + CLI, Bun) — reference for the most feature-complete OSS digest Action. Multi-mode, multi-LLM, output language, cache, retries, filters.
- `sahansera/gh-weekly-updates` (Python CLI) — auto-discover repos, custom prompt file, push to repo. The single-audience model for our `team` report.
- `evoleinik/changelog-summary` (GitHub Action) — **defines voice styles "founder / developer / marketing" from same data**, the closest analog to our two-prompt framing.
- `yinzhenyu-su/weekly-git-summary` (cross-platform CLI) — local git commit summariser.
- `kestra` blueprint "Summarize Weekly Git Commits with OpenAI" — confirms the standard workflow: clone → collect → summarise → push.

**Web searches / comparisons cross-checked:**
- devopsschool.com — "Top 10 AI Release Notes & Changelog Generators" — categorises LaunchNotes, ReleaseNotes.io, GitHub Automated Release Notes, Release Please, GitClear, Beamer, Headway, LogRocket Galileo AI, Conventional Changelog, ChangeCrab.
- arxiv 2505.17977 — *SmartNote: An LLM-Powered, Personalised Release Note Generator* — research backing for audience-framed release notes.
- arxiv 2204.05355 — *Exploring Relevant Artifacts of Release Notes* — practitioner evidence that release notes are produced for both internal and external users with different framings.
- uplevelteam.com / gitvelocity.dev / oobeya.io (2026 buyer's guides) — engineering analytics (Jellyfish, LinearB, Swarmia, DX, Waydev) all target DORA metrics, not narrative digests → confirms the gap.
- launchnotes.com — "Release Notes vs Changelog: Key Differences" — explicit framing of internal-vs-customer release notes.

**Confidence notes:**
- High confidence on the feature *list* (table stakes / differentiators / anti-features) — triangulated from ≥3 independent sources.
- Medium confidence on the *priorities* — they reflect this project's documented constraints (`.planning/PROJECT.md`) and should be re-checked after MVP runs once.
- The "no inspected tool does our exact gap" claim is positive-citable against ~6 OSS tools + 10 SaaS / analytics competitors; it is *likely* to miss a private/internal competitor, but that's acceptable for an internal CLI whose threat model is not competition.

---
*Feature research for: summarizer (GitLab project summarizer)*
*Researched: 2026-10-20*
