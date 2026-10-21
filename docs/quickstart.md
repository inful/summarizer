# Quickstart

The 30-second version. If you've never run summarizer before, this is
where to start.

## Install

```bash
go install github.com/inful/summarizer/cmd/summarizer@latest
```

Or build from source:

```bash
git clone https://github.com/inful/summarizer
cd summarizer && go build -o bin/summarizer ./cmd/summarizer
```

## Configure

Two environment variables (secrets — never put these in `summarizer.yaml`):

```bash
export GITLAB_TOKEN="glpat-..."          # required; PAT with read_api on the source project
export OPENAI_API_KEY="sk-..."           # required when --provider openai (default)
```

A minimal config:

```yaml
# summarizer.yaml — copy to ./summarizer.yaml or pass --config /path/
gitlab_host: "https://gitlab.example.com"
```

The project set is a CLI flag, not a config key. With no discovery
flag, summarizer runs **every project the token can see that had
activity in the window** — no topic, no config entry needed. A project
opts out with the `no-summarize` topic or a `.summaryignore` /
`.docignore` file at its repo root. Both also win over an explicit
`--project` listing. The reports project itself is skipped too, so
`publish.project` never summarises itself. To narrow the scope instead:

| Flag | Selects |
|---|---|
| *(none)* | every project with activity in the window |
| `--discover-active` | the same, stated explicitly |
| `--discover-group G` | only projects in group `G` |
| `--discover-by-topic` | only projects carrying a `summarize*` topic |
| `--project P` | exactly `P` |

Customer-facing output is separate: every project that runs gets a
team report, and only projects carrying the `summarize` topic also get
a customer one.

## First run — dry

```bash
summarizer run --project group/subgroup/project --dry-run
```

Fetches this ISO week so far from GitLab and prints a one-line
summary to stderr. No LLM call, no file write. Tells you whether the
token works and the project path resolves.

For the real default, drop `--project` to see the activity scan and
read the `discovery summary included=N excluded=M` line before letting
it publish:

```bash
summarizer run --dry-run
```

## Real run

```bash
summarizer run --project group/subgroup/project
```

Produces two Markdown files in `./summarizer-out/`:

```
group_subgroup_project--wk--customer.md
group_subgroup_project--wk--team.md
```

Both have a one-line run summary on stderr:

```
summarizer: ok — 1 project(s), 47 event(s) (47 used), 2 file(s) written (window=this ISO week (since Monday 00:00 UTC, 60h0m0s elapsed))
  → ./summarizer-out/2026-W43/group_subgroup_project--wk--customer.md
  → ./summarizer-out/2026-W43/group_subgroup_project--wk--team.md
  tokens[group/subgroup/project/customer]: prompt=2103 completion=812 total=2915
  tokens[group/subgroup/project/team]:     prompt=2478 completion=901 total=3379
```

## Cron

```cron
0 23 * * 0 cd /path/to/summarizer && ./bin/summarizer run --config ./summarizer.yaml
```

(Sunday 23:00 UTC — the last day of the ISO week, so a single run
covers the whole week. A Monday run would summarise the new week
instead of the one that just ended. See `docs/runbook.md`.)

Reports land under a week-numbered directory — `./summarizer-out/2026-W43/`
locally, `<prefix>/2026-W43/` in the target repo. Every run inside the
same Mon–Sun week overwrites that directory, so a daily cron refines
one week's report instead of opening seven.

## Month end

Once weekly reports are being published to a GitLab target repo:

```bash
summarizer monthly                    # previous calendar month
summarizer monthly --month 2026-09    # re-run a past month
```

Reads last month's published weekly rollups and writes one month-end
file per audience. Needs `publish.enabled: true` in the config.

## Next

- `README.md` — full feature reference, exit codes, output layout, local-provider config
- `docs/how-it-works.md` — data flow + component map; useful if you're going to extend
- `docs/frontmatter-schema.md` — every docbuilder key in the published output; for docbuilder operators
- `docs/runbook.md` — what to do when X breaks; for the operator on call
