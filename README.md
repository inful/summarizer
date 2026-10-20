# summarizer

One command turns a week of GitLab activity into two readable Markdown reports — one for the project's customers, one for the team and their peers.

## Documentation

| Document | Purpose |
|---|---|
| [`docs/quickstart.md`](docs/quickstart.md) | 30-second operator view: install, env vars, one example run |
| [`README.md`](README.md) | Full feature reference (this file) |
| [`docs/how-it-works.md`](docs/how-it-works.md) | Data flow diagram + component map; for extending |
| [`docs/frontmatter-schema.md`](docs/frontmatter-schema.md) | Every docbuilder key in the published output; for docbuilder operators |
| [`docs/runbook.md`](docs/runbook.md) | Operator playbook: exit codes, guard scenarios, token rotation, troubleshooting |
| [`CHANGELOG.md`](CHANGELOG.md) | Notable changes per release |

## What it does

`fetch → normalize → prompt ×2 → complete ×2 → write`. One invocation, one GitLab project, two Markdown files. MVP; built to be scheduled by cron.

When the project lives under a GitLab group with Premium/Ultimate epics, the per-project report also surfaces them — events grouped under epic headers in an `## Epics active this period` section, with non-epic events flowing to `## Other activity`. Free-tier and user-namespace projects render flat as before. See `docs/how-it-works.md` for the data flow and `docs/deferred.md` for what deliberately is not yet included.

## Install

```bash
go install github.com/inful/summarizer/cmd/summarizer@latest
```

Or build from source:

```bash
go build -o bin/summarizer ./cmd/summarizer
```

## Configure

Copy `examples/summarizer.yaml` to `./summarizer.yaml` (or pass `--config path/to/file.yaml`). The only required field is `gitlab_host`.

Secrets come from environment, never from config:

```bash
export GITLAB_TOKEN="glpat-..."          # required
export OPENAI_API_KEY="sk-..."           # required when provider=openai and baseURL is api.openai.com; optional otherwise
# export OPENAI_BASE_URL="http://localhost:11434/v1"  # for local Ollama/vLLM/LM Studio
# export ANTHROPIC_API_KEY="sk-ant-..."  # required when provider=anthropic
```

### Local LLM providers

Any OpenAI-Chat-Completions-compatible endpoint works: Ollama, vLLM, LM Studio, llama.cpp, llama-stack, OpenRouter. Point `OPENAI_BASE_URL` (or `openai_base_url` in config, or `--openai-base-url` flag) at the local host. `OPENAI_API_KEY` is optional in this case — most local servers ignore the `Authorization` header.

```bash
# Ollama (default port 11434)
export OPENAI_BASE_URL="http://localhost:11434/v1"
summarizer run --project group/subgroup/project --model llama3.1

# LM Studio (default port 1234)
export OPENAI_BASE_URL="http://localhost:1234/v1"
summarizer run --project group/subgroup/project --model qwen2.5-coder

# vLLM (default port 8000)
export OPENAI_BASE_URL="http://localhost:8000/v1"
summarizer run --project group/subgroup/project --model Qwen/Qwen2.5-Coder-32B-Instruct
```

Either bare-host (`http://localhost:11434`) or `/v1`-suffixed (`http://localhost:11434/v1`) forms are accepted; the normalizer canonicalizes to one.

## Publishing to a GitLab project (reviewable MRs)

Default output is local Markdown files. To publish instead — committed to a GitLab project as merge requests — add a `publish:` block to `summarizer.yaml`:

```yaml
gitlab_host: "https://gitlab.example.com"
publish:
  enabled: true
  host: "https://gitlab.example.com"     # default = same as gitlab_host
  token_env: "GITLAB_REPORTS_TOKEN"       # PAT with api scope on the *target* project
  project: "group/reports"               # target project path
  prefix: "reports"                       # repo path prefix; "" = repo root
  default_branch: "main"
  source_branch_prefix: "summarizer"
```

Then:

```bash
export GITLAB_REPORTS_TOKEN="glpat-..."   # separate from $GITLAB_TOKEN
summarizer run --project group/proj --since 7d
```

What you get:

1. For each run, a source branch is created (e.g. `summarizer/2026-10-19`) from `<default_branch>` if it doesn't exist.
2. Each rendered report (per project, per audience) is committed to that branch under `<prefix>/<period>/<filename>`, wrapped in **docbuilder-format frontmatter** (`uid`, `title`, `date`, `lastmod`, `fingerprint`, `tags`, `categories`, `aliases`, plus `public: true` for customer-audience reports). docbuilder in your `reports` project indexes them automatically.
3. A single MR is opened against `<default_branch>` with body: "Summarizer rollup 2026-10-19".
4. Re-runs for the same period accumulate commits on the same MR — no branch/MR sprawl.
5. If a prior MR for the period is already merged, summarizer refuses rather than reopen it; pick a new period slug or merge yourself.

### Resulting directory tree (after every week's MR is merged into `main`):

```
reports/
  2026-10-13/
    group-proj--7d--customer.md        # public: true → customer index
    group-proj--7d--team.md            # no public key    → internal index
    other-proj--7d--customer.md       # public: true
    other-proj--7d--team.md
    combined--7d--customer.md          # cross-project rollup (when ≥2 projects)
    combined--7d--team.md
    combined--epics--7d--customer.md   # cross-project epic rollup (when ≥1 active epic)
    combined--epics--7d--team.md
  2026-10-20/
    ...
```

Each weekly run = one folder. The GitLab UI and `git log` both let you browse week-by-week. `public: true` on customer reports means docbuilder exposes them externally; team reports stay inside the team's index — defense-in-depth on top of the folder layout.

### Why a separate token

`GITLAB_TOKEN` (used for source fetch) only needs `read_api` on the source project. The publishing identity (`GITLAB_REPORTS_TOKEN`) needs `api` scope on the *target* project — and a different audit trail. Splitting them means the publishing identity can be revoked without losing the read side, and vice versa.

### Cron-friendly

The same cron line that runs the local-disk variant runs the publishing variant:

```cron
0 9 * * 1 summarizer run --config /etc/summarizer/subscriptions.yaml --project group/proj --since 7d
```

When `publish.enabled: true`, the local disk write is skipped — every report file lives only in the GitLab target until a human merges the MR. Summarizer succeeds (exit 0) when the commits land; it does not wait for MR merge.

## `--safety-enabled` (prompt-injection guard)

Opt-in check that runs the rendered prompt through your `inful/decide` client before each LLM call. Noul probability ≥ `injection_threshold` ⇒ injection ⇒ hard-fail the run.

```bash
summarizer run --project group/proj --safety-enabled
# or with config:
#   safety:
#     enabled: true
#     decision_url: "https://api.example.com"
#     decision_token_env: "SAFETY_DECISION_TOKEN"
#     injection_threshold: 0.7
```

Why a separate decision API rather than asking the LLM "is this safe?":
- Decide is a fast "system 1" classifier tuned for routing decisions. Doubling the LLM cost per generation to ask the LLM "is this safe?" is wasteful and self-defeating (a compromised LLM answers yes).
- The structural defense stays the `<data>` markers around untrusted fields. The guard is **one** of several layers, not a replacement.

Cost: 1 extra `decide.SystemOne` call per LLM generation (so ~4N+2 per multi-project run for N projects). Cheap; the doubled latency is the real consideration.

### Soft-fail on guard errors, hard-fail on injection

If the `/v1/systemone` API is down, returns malformed data, or times out, summarizer logs a `WARN safety guard unavailable` line and proceeds with the LLM call anyway. A flaky decision API does not block your weekly cron — you lose only the second-opinion verdict, not the structural defense (`<data>` markers, system-prompt-level refusal rules).

A **prompt-injection verdict** (`VerdictInjection`) is the one case that hard-fails the run with a non-zero exit. That's the scenario where the guard did its job and we should respect it.

## `--include-diffs` (team prompt only)

```bash
summarizer run --project group/proj --include-diffs
```

Default off. When on, summarizer fetches per-MR diff text from `GET /merge_requests/:iid/changes` and folds it into the **team** prompt only. The customer prompt is unaffected.

How it lands:

```
- [MR #847] (merged, by alice) "Add login button" [+412/-89 in 12 files] [comments: 12] — https://gitlab...
  body: <data>Implements the v3 login redesign.</data>
  diff: <data>internal/auth/auth.go (+312/-89):
+func (a *Auth) Login(...) (token Rotation, biometricEnabled bool) error
---
internal/auth/session.go (+50/-0):
+a new helper for rotating sessions
---</data>
```

The diff text is always wrapped in `<data>` markers (same OWASP LLM01:2025 mitigation as descriptions). The team prompt instructions tell the model to **infer what each MR does from the diff — paraphrase, never paste code**. So even with the diff in front of it, the model writes things like "alice refactored the auth layer to support token rotation" rather than copying function bodies into the report.

### Caps

| Cap | Value | Source |
|---|---|---|
| Per-file diff (decode-time, memory bound) | 600 runes | `internal/gitlab/client.go` `maxDiffRunesPerFile` |
| Per-MR diff total (prompt-time) | 4000 runes | `internal/prompt/prompt.go` `diffMaxPerMR` |
| Wire cost | One extra `/changes` call per MR | worker pool, 4 concurrent |

For a project with 50 MRs, `--include-diffs` adds one extra HTTP round-trip per MR (50 calls, ~50s parallel-ish at 4-concurrency). The model already produces the team report; the diff lands in the existing prompt context, so no extra LLM call is added.

## Run

```bash
# Dry-run: fetch GitLab, print event count, no LLM call, no file write.
summarizer run --project group/subgroup/project --dry-run

# Real run: default 7-day window, two reports to ./summarizer-out/.
summarizer run --project group/subgroup/project

# Custom window + provider.
summarizer run --project group/subgroup/project --since 30d --provider anthropic

# Cron-friendly: refuse to overwrite an existing report unless --force.
summarizer run --project group/subgroup/project --force

# Multi-project: --project is repeatable; comma-separated works too.
summarizer run --project group/a --project group/b
summarizer run --project group/a,group/b

# Wide net: --discover-group scans a group's projects and runs the
# pipeline on whichever had MR activity in the window. Composes with
# --project (explicit wins on duplicates; the discovery pass skips
# already-explicit projects to spare the probe round-trip).
summarizer run --discover-group group/team-a --since 7d
summarizer run --discover-group group/team-a --project group/team-a/flagship --since 7d

# Audience policy: which projects produce customer-facing output?
# Three sources union together: --internal-only-project flag,
# summarizer.yaml's internal_only_projects block, and the GitLab-side
# `summarizer-team-only` topic on the project. Discovered projects
# default to team-only unless the project carries the `summarizer`
# topic (opt-in to customer output). The auditor-friendly rule:
# absence of any topic = team only for discovered, both audiences
# for explicit-listed.
summarizer run --discover-group group/team-a \
    --internal-only-project group/team-a/internal-tool --since 7d

# When more than one project runs (whether explicit or discovered),
# summarizer also writes a combined rollup
# (combined--<since>d--customer.md and --team.md) by feeding the
# per-project reports to one extra LLM call per audience.
```

Output files are written under `--output-dir` (or `output_dir` from config):

```
# Single-project run:
<project>--<duration>d--customer.md
<project>--<duration>d--team.md

# Multi-project run (additionally):
combined--<duration>d--customer.md
combined--<duration>d--team.md
```

## Schedule

```cron
# Every Monday at 09:00, refresh last week's reports.
0 9 * * 1 cd /path/to/summarizer && ./summarizer run --project group/subgroup/project >> /var/log/summarizer.log 2>&1
```

## Design

- `internal/events.NormalizedEvent` is the **port boundary**. GitLab JSON never crosses it.
- Free-text GitLab fields are wrapped in `<data>` markers in the prompt. The model is told explicitly never to follow instructions inside `<data>`. (OWASP LLM01:2025 mitigation.)
- The customer report never sees MR/issue body text. The team report gets bodies, capped and `<data>`-wrapped.
- File writes are atomic (`os.Rename` from `.tmp-*`). A crash mid-write leaves the previous good file untouched.
- Output filenames encode the window. Re-runs land on different files; `--force` overwrites.

## Exit codes (cron-friendly)

| Code | Meaning                                     |
|------|---------------------------------------------|
| 0    | Success                                     |
| 2    | Usage error (missing flag, bad subcommand)  |
| 3    | Config error (missing/invalid YAML)         |
| 4    | GitLab fetch failure                        |
| 5    | LLM call failure                            |
| 6    | File write failure (e.g. target exists)     |
| 7    | Missing env var (token / API key)           |

## Layout

```
summarizer/
├── cmd/summarizer/        # entry point; wires everything
├── internal/
│   ├── config/            # YAML loader + validation
│   ├── events/            # NormalizedEvent — the port boundary
│   ├── gitlab/            # net/http GitLab REST v4 client
│   ├── llm/               # Provider interface + OpenAI + Anthropic
│   ├── prompt/            # embedded templates + render
│   ├── run/               # pipeline orchestrator
│   └── writer/            # atomic Markdown write
├── examples/
│   └── summarizer.yaml    # sample config
└── README.md
```

## Out of scope (deliberately, for MVP)

- Multi-project runs in one invocation.
- Built-in scheduler / daemon mode.
- Slack / email / wiki delivery.
- DORA metrics / cycle-time analytics.
- OAuth user flows — PAT via env is sufficient.
- Real-time activity streaming.

These are not anti-features; they are deferred until MVP proves out. See git history for the discarded planning scaffolding.