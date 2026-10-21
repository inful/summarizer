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
flag, summarizer runs every project the token can see that had
activity in the window; `--project`, `--discover-group` and
`--discover-by-topic` narrow or redirect that. See the README.

## First run — dry

```bash
summarizer run --project group/subgroup/project --dry-run
```

Fetches the last 7 days of activity from GitLab and prints a one-line
summary to stderr. No LLM call, no file write. Tells you whether the
token works and the project path resolves.

## Real run

```bash
summarizer run --project group/subgroup/project --since 7d
```

Produces two Markdown files in `./summarizer-out/`:

```
group_subgroup_project--7d--customer.md
group_subgroup_project--7d--team.md
```

Both have a one-line run summary on stderr:

```
summarizer: ok — 1 project(s), 47 event(s) (47 used), 2 file(s) written (window=168h0m0s)
  → ./summarizer-out/group_subgroup_project--7d--customer.md
  → ./summarizer-out/group_subgroup_project--7d--team.md
  tokens[group/subgroup/project/customer]: prompt=2103 completion=812 total=2915
  tokens[group/subgroup/project/team]:     prompt=2478 completion=901 total=3379
```

## Cron

```cron
0 9 * * 1 cd /path/to/summarizer && ./bin/summarizer run --config ./summarizer.yaml
```

(Monday morning, 09:00 UTC.)

## Next

- `README.md` — full feature reference, exit codes, output layout, local-provider config
- `docs/how-it-works.md` — data flow + component map; useful if you're going to extend
- `docs/frontmatter-schema.md` — every docbuilder key in the published output; for docbuilder operators
- `docs/runbook.md` — what to do when X breaks; for the operator on call
