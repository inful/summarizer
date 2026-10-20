# summarizer

One command turns a week of GitLab activity into two readable Markdown reports — one for the project's customers, one for the team and their peers.

## What it does

`fetch → normalize → prompt ×2 → complete ×2 → write`. One invocation, one GitLab project, two Markdown files. MVP; built to be scheduled by cron.

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

# When --project appears more than once, summarizer also writes a
# combined rollup (combined--<since>d--customer.md and --team.md) by
# feeding the per-project reports to one extra LLM call per audience.
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