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
export OPENAI_API_KEY="sk-..."           # required for provider=openai (default)
# export ANTHROPIC_API_KEY="sk-ant-..."  # required for provider=anthropic
```

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
```

Output files are written under `--output-dir` (or `output_dir` from config) as:

```
<project>--<since-days>d--customer.md
<project>--<since-days>d--team.md
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