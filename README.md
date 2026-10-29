# summarizer

One command turns a week of GitLab activity into two readable Markdown reports — one for the project's customers, one for the team and their peers.

## Install

### Binary

Download the archive for your platform from the [latest release](https://github.com/inful/summarizer/releases/latest), extract, and place `summarizer` on your `$PATH`.

### Docker

```bash
docker pull ghcr.io/inful/summarizer:latest
docker run --rm ghcr.io/inful/summarizer --help
```

The image defaults to the `summarizer` entrypoint, so any of the documented subcommands work as arguments. It runs as uid 65532, so a bind-mounted host directory must be writable by that uid — either `chown 65532 ./reports` first, or add `--user "$(id -u):$(id -g)"` to the run. Without one of those the write fails with `permission denied` on a directory owned by your own user.

To write reports into a host directory, mount it to the path you pass via `--output-dir`:

```bash
docker run --rm \
  -v "$PWD/reports:/out" \
  -e GITLAB_TOKEN=$GITLAB_TOKEN \
  -e OPENAI_API_KEY=$OPENAI_API_KEY \
  ghcr.io/inful/summarizer:latest \
    run --project group/sub/proj --output-dir /out
```

A debug variant is published alongside each release with a busybox shell
layered in (CI use, or `docker exec` for debugging). It is tagged
`:latest-debug` and `:<version>-debug` — there is no plain `:debug` tag:

```bash
docker pull ghcr.io/inful/summarizer:latest-debug
docker run --rm --entrypoint /busybox/sh ghcr.io/inful/summarizer:latest-debug -c 'id'
```

`--entrypoint` is required: the image's entrypoint is the binary, so
without it `/busybox/sh` is passed to `summarizer` as an argument and
the binary prints its usage instead of giving you a shell.

### From source

```bash
go install github.com/inful/summarizer/cmd/summarizer@latest
```

`@latest` builds a `dev` binary (no version baked in). To get a real version, build with `goreleaser release --snapshot --skip=publish --skip=docker`, or set ldflags explicitly:

```bash
go build -ldflags "-X main.version=vX.Y.Z -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -trimpath -o summarizer ./cmd/summarizer
```

## Documentation

| Document | Purpose |
|---|---|
| [`docs/quickstart.md`](docs/quickstart.md) | 30-second operator view: install, env vars, one example run |
| [`docs/kubernetes.md`](docs/kubernetes.md) | CronJob deployment: manifest, secrets, scheduling, exit codes |
| [`README.md`](README.md) | Full feature reference (this file) |
| [`docs/how-it-works.md`](docs/how-it-works.md) | Data flow diagram + component map; for extending |
| [`docs/frontmatter-schema.md`](docs/frontmatter-schema.md) | Every docbuilder key in the published output; for docbuilder operators |
| [`docs/runbook.md`](docs/runbook.md) | Operator playbook: exit codes, guard scenarios, token rotation, troubleshooting |
| [`CHANGELOG.md`](CHANGELOG.md) | Notable changes per release |

## What it does

`fetch → normalize → prompt ×2 → complete ×2 → write`. One invocation, one GitLab project, two Markdown files. MVP; built to be scheduled by cron.

When the project lives under a GitLab group with Premium/Ultimate epics, the per-project report also surfaces them — events grouped under epic headers in an `## Epics active this period` section, with non-epic events flowing to `## Other activity`. Free-tier and user-namespace projects render flat as before. See `docs/how-it-works.md` for the data flow and `docs/deferred.md` for what deliberately is not yet included.

### Confidential work stays out of customer-facing output

GitLab marks issues and epics `confidential` — the owner's own "not for
everyone". A token with `read_api` on the project receives them from
the ordinary list endpoints, exactly like any other issue.

They are dropped from **customer-facing** output: a confidential issue,
an issue inside a confidential epic, and the confidential epic's own
heading are all removed before the customer prompt is rendered, so they
cannot reach the customer report — which is published `public: true` —
or the cross-project and epic rollups built from it.

The **team-facing** output keeps them. That document is internal, and
the team is who marked the work.

> Before v0.3.0 this was only true for epics, and only in the epic
> rollup. A confidential issue's *title* reached the customer report,
> silently, with no warning and a zero exit code. See the v0.3.0
> changelog.

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
summarizer run --project group/proj
```

What you get:

1. For each run, a source branch is created (e.g. `summarizer/2026-W43`) from `<default_branch>` if it doesn't exist. The window is **this ISO week** — Monday 00:00 UTC to now — so the branch named `2026-W43` holds Monday-to-now of week 43 and nothing else. There is no `--since`; one window means a filename, a directory and its contents can never disagree.
2. Each rendered report (per project, per audience) is committed to that branch under `<prefix>/<period>/<filename>`, wrapped in **docbuilder-format frontmatter** (`uid`, `title`, `date`, `lastmod`, `fingerprint`, `tags`, `categories`, `aliases`, plus `public: true` for customer-audience reports). docbuilder in your `reports` project indexes them automatically.
3. A single MR is opened against `<default_branch>` with body: "Summarizer rollup 2026-W43".
4. Re-runs for the same period accumulate commits on the same MR — no branch/MR sprawl.
5. If the period's MR was already **merged or closed**, the re-run still
   writes — but onto a branch that will never merge again, so nothing
   reaches `main`. That is a logged no-op, not a failure: it is the
   normal response to a report that said the wrong thing. The run
   summary says how many files fell in that case, e.g. `4 file(s)
   written (2 already published this week)`.

### Resulting directory tree (after every week's MR is merged into `main`):

```
reports/
  2026-W42/
    group-proj--wk--customer.md        # public: true → customer index
    group-proj--wk--team.md            # no public key    → internal index
    other-proj--wk--customer.md       # public: true
    other-proj--wk--team.md
    combined--rollup--wk--customer.md   # cross-project rollup (when ≥2 projects)
    combined--rollup--wk--team.md
    combined--epics-rollup--wk--team.md        # whole fleet, team only
    combined--epics-<customer-slug>--wk--customer.md   # one per attributed customer
  2026-W43/
    ...
  2026-09/                             # `summarizer monthly` output
    monthly--2026-09--customer.md
    monthly--2026-09--team.md
```

Each weekly run = one folder, named by ISO week. The GitLab UI and `git log` both let you browse week-by-week. `public: true` on customer reports means docbuilder exposes them externally; team reports stay inside the team's index — defense-in-depth on top of the folder layout.

### Why a separate token

`GITLAB_TOKEN` (used for source fetch) only needs `read_api` on the source project. The publishing identity (`GITLAB_REPORTS_TOKEN`) needs `api` scope on the *target* project — and a different audit trail. Splitting them means the publishing identity can be revoked without losing the read side, and vice versa.

### Cron-friendly

The same cron line that runs the local-disk variant runs the publishing variant:

```cron
0 23 * * 0 summarizer run --config /etc/summarizer/subscriptions.yaml --project group/proj
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

## Activity triage (classify the week before the prompt)

Opt-in. Each project's week is classified with your `inful/decide` API **before** the prompt is rendered, and the verdict becomes a note the prompt carries.

```bash
# config only — there is no CLI flag for this
#   triage:
#     enabled: true
#     decision_url: "https://api.example.com"   # defaults to safety.decision_url
#     decision_token_env: "SAFETY_DECISION_TOKEN"  # defaults to safety.decision_token_env
#     mode: "week"                              # "week" (default) or "mr"
```

**Why.** The team report asks "what is blocked" — and nothing in the pipeline used to collect anything that *meant* blocked. So the model either invented blockers or wrote "nothing appears blocked", and neither is an answer. A classifier answers that question; the model, which sees every event, still says *which* item is blocked and what it waits on.

That division is not a limitation of the design, it is the division of labour: a `noul` is one probability for the whole state, so it can say "there is blocked work this week" and never "MR #7 is blocked". The classifier decides **whether to look**; the LLM finds **which one**.

**What the classifier can and cannot reach.** The note is team-only. It never appears in a customer-facing document — those are published `public: true`, and "our team is blocked on X" is not a customer update.

The classifier itself sees the project's **full** activity, including work marked confidential — it is the same service the safety guard already sees the whole rendered prompt through, and a classification that omitted half the week would be a worse answer than one that saw all of it. What the confidential work can reach is the internal team report, which is where it already belongs.

**Two call shapes, and they cost very differently:**

| mode | calls per project | what it can answer | cost |
|------|-------------------|--------------------|------|
| `week` (default) | 1 | "is there blocked work this week?" | flat — does not scale with the size of the week |
| `mr` | one per merge request | per-item answers | one call per event |

Measured against a 4K-context stand-in, `mr` cost 155 calls / 57.5k input tokens / 6m16s for one project, where `week` cost 1 call / 2k tokens / 4.5s. Whether `mr` is viable depends entirely on your decision API's latency and price per token — so the run summary reports what the classification actually cost:

```
  triage[group/proj]: 155 call(s), 57505 in / 20835 out
```

**Soft-fail on classifier errors.** If the decision API is down, summarizer logs `WARN triage unavailable for <project> — proceeding without the classifier hint` and the project still publishes. The report is then missing its blockers section, which is why the summary also prints `triage[...]: FAILED` — a report that silently lost its hint reads exactly like a report that found nothing.

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

Every cap names its unit, and the units are **not** uniform: four of
these count runes and three count bytes. That is not a typo to be tidied
away — `formatDiffChunks` slices a string by byte offset and
`maxDiffRunesPerFile` truncates by rune, and when the per-chunk cap
counted runes while the per-MR cap counted bytes the slice landed inside
a multi-byte character and put invalid UTF-8 in the team prompt. Both
now respect rune boundaries; the units still differ because each cap is
applied in the unit its own code uses.

Five of them are yours to set, in `summarizer.yaml` (and `--max-events`
on the command line). The rest are listed so you know what they are:
most are structural bounds on what the tool talks to, and two bound the
text the model reads.

| Cap | Default | Unit | Config key | Source |
|---|---|---|---|---|
| Projects per run | 50 | count | `--max-projects` | `internal/run/pipeline.go` `DefaultMaxProjects` |
| Events per project in the prompt | 200 | count | `max_events` / `--max-events` | `internal/prompt/prompt.go` `capEvents` |
| **Completion tokens per LLM call** | **4096** | tokens | `max_tokens` | `internal/llm/provider.go` `DefaultMaxTokens` |
| MR / issue / commit title | 200 | **runes** | — | `internal/events/normalize.go` `TitleCap` |
| MR / issue body (team prompt only) | 600 | **runes** | — | `internal/events/normalize.go` `DescriptionCap` |
| Event list pages per project per week | 20 pages = 2,000 records | count | — | `internal/gitlab/client.go` `maxEventPages` |
| Active-project scan pages | 10 pages = 1,000 candidates | count | — | `internal/gitlab/client.go` `maxActiveScanPages` |
| Active epics per project in the epic rollup | 20 | count | — | `internal/run/pipeline.go` `maxActiveEpics` |
| Per-file diff (decode-time, memory bound) | 600 | **runes** | `max_diff_runes_per_file` | `internal/gitlab/client.go` `maxDiffRunesPerFile` |
| Rollup body, per input | 4,000 | **runes** | `rollup_body_cap` | `internal/prompt/prompt.go` `DefaultRollupPerBody` |
| Rollup body, total across all inputs | 16,000, least-active inputs dropped first | **bytes** | `rollup_total_cap` | `internal/prompt/prompt.go` `DefaultRollupTotal` |
| Per-MR diff total (prompt-time) | 4,000 | **bytes** | `diff_max_per_mr` | `internal/prompt/prompt.go` `DefaultDiffMaxPerMR` |
| HTTP response body read | 16 MB (GitLab, publisher), 8 MB (LLM) | bytes | — | `maxResponseBytes` |

**The title and body caps bound what the model reads.** They are not
structural: a 250-rune MR title is cut to 200 and a 700-rune body to 600
before the prompt is built, so a long title can arrive saying less than
it says. They are fixed constants, not config keys — if you need a
longer title or body, that is a change to `internal/events`, not to
`summarizer.yaml`.

**`max_tokens` is the one that bites.** A completion that hits the cap
is *detected* — both providers return a truncation error rather than a
finished-looking fragment — so it fails the project, and a failed
project means the cross-project rollups are skipped for the week (a
rollup over a partial set would silently read as a complete week). The
cap was 2048 and a busy week could reach it; it is 4096 now.

The 16,000-byte total is the rollup's whole budget across every input,
so on a wide run most projects never reach it — measured at 20 projects
and ~2 KB reports, 8 bodies carried and 12 dropped. The prompt header
says so explicitly (`Projects in this rollup: 20 (8 shown, 12 omitted
for size)`) and every input is still named, so the published file
cannot read as a complete week when it is not.
| Wire cost | One `/changes` call per MR, made either way | worker pool, 4 concurrent |

`capEvents` keeps the most recently **updated** events, not the first
N in fetch order — MRs, issues and commits arrive as three separate
batches, so a head-slice would starve commits whenever MRs and issues
already filled the budget.

`--include-diffs` adds **no** API calls. The `/changes` call is made for every MR regardless, because the team prompt renders a `+N/-M in K files` aggregate from the same response; the flag only decides whether the diff text is kept. Turning it off saves nothing on the wire and costs the team report its code-level detail. It adds no LLM call either — the diff lands in the existing prompt context.

That call is the run's largest wire cost by a wide margin. Measured against a 25-project fleet with 20 MRs each: **676 requests, 500 of them `/changes` — 74%**, one per MR. It is bounded by `max_events`, which the pipeline applies *before* the fetch, so an MR the cap would drop anyway never costs a request. `--dry-run` stops before it, so a preview understates a real run by roughly three-quarters.

## Run

```bash
# Dry-run: fetch GitLab, print event count, no LLM call, no file write.
summarizer run --project group/subgroup/project --dry-run

# Real run: this ISO week, two reports to ./summarizer-out/.
summarizer run --project group/subgroup/project

# Switch provider. The model default is per-provider, so this needs no
# --model; set one in summarizer.yaml if you want something else.
summarizer run --project group/subgroup/project --provider anthropic

# Cron-friendly: period buckets overwrite in place, so a second run of
# the same week rewrites the same path rather than piling up files.
summarizer run --project group/subgroup/project

# Multi-project: --project is repeatable; comma-separated works too.
summarizer run --project group/a --project group/b
summarizer run --project group/a,group/b

# Default: run every project the token can see that had activity in
# the window. No topic, no config entry. This is what happens when no
# discovery flag is passed at all, and it's what --discover-active
# asks for explicitly.
summarizer run
summarizer run --discover-active

# Narrower: --discover-group scans one group's projects and runs the
# pipeline on whichever had MR or issue activity in the window.
# Composes with --project (explicit wins on duplicates; the discovery
# pass skips already-explicit projects to spare the probe round-trip).
summarizer run --discover-group group/team-a
summarizer run --discover-group group/team-a --project group/team-a/flagship

# Server-wide by topic: --discover-by-topic picks up every project
# on the instance carrying a `summarize` or `summarize-team-only`
# topic, across all groups the token can see. Kept as an escape
# hatch for instances where the active-project net is too wide.
summarizer run --discover-by-topic
summarizer run --discover-by-topic --discover-group group/team-a

# Opting out: two project-owner signals, either sufficient.
#
# 1. The `no-summarize` GitLab topic — a one-click reversible toggle,
#    and free to check because the topics already arrive with the
#    project summary. Use it for "noisy this sprint" or a repo you
#    administer in GitLab but shouldn't be pushing branches into.
#
# 2. A `.summaryignore` or `.docignore` file in the project repo root.
#    The better signal: version-controlled, greppable, visible to
#    everyone who clones the repo. It costs a commit and a merge.
#      touch .summaryignore && git add .summaryignore && git commit -m "skip summarizer"
#
# Both win over discovery AND over an explicit --project listing: a
# cron line is not a licence to publish something a project asked not
# to be published. The discovery log names which signal fired.

# Audience policy: every project that runs produces a team-only
# report. Customer-facing output needs the `summarize` topic on the
# project. Three sources can force team-only and they union
# together: --internal-only-project, summarizer.yaml's
# internal_only_projects block, and the `summarize-team-only` topic.
# Project paths are matched case-insensitively and YAML values are
# trimmed, because GitLab's own project routes are: a spelling
# difference between --project and --internal-only-project would
# otherwise mark the same project both ways at once.
summarizer run --discover-group group/team-a \
    --internal-only-project group/team-a/internal-tool

# When more than one project runs (whether explicit or discovered),
# summarizer also writes a combined rollup
# (combined--rollup--wk--customer.md and --team.md) by feeding
# the per-project reports to one extra LLM call per audience. Active
# epics add one customer file per attributed customer,
# combined--epics-<customer-slug>--*, where an epic or any of its
# issues carries an org::CODE label. Epics nobody attributed appear in
# the TEAM rollup only — customer output is opt-in everywhere else in
# summarizer, and an unattributed epic is the least attributed thing
# there is. An epic only appears under a project that actually saw
# activity on it: epics are group-scoped, so every project in a group
# sees the same epic list, and the rollups skip the ones with no events
# here rather than listing them as "0 events".

# Period bucketing is by ISO week number, and the default window is
# that same week. Every run inside one Mon-Sun week writes to
# <prefix>/<YYYY-Www>/, onto the same source branch, updating the same
# MR — so a daily cron refines one week's report instead of opening
# seven directories a week. A single Sunday-night run covers the week
# in one go, which is what the cron examples above do.
summarizer run --discover-group group/team-a
```

Output files are written under `--output-dir` (or `output_dir` from config),
one subdirectory per ISO week — the same layout the GitLab publisher
uses in the target repo:

```
# Single-project run:
<output_dir>/2026-W43/<project>--wk--customer.md
<output_dir>/2026-W43/<project>--wk--team.md

# Multi-project run (additionally):
<output_dir>/2026-W43/combined--rollup--wk--customer.md
<output_dir>/2026-W43/combined--rollup--wk--team.md

# With active epics (additionally):
<output_dir>/2026-W43/combined--epics-rollup--wk--team.md
<output_dir>/2026-W43/combined--epics-<customer-slug>--wk--customer.md
```

Runs inside the same week overwrite in place (that is the point of
weekly buckets); each new week gets its own directory, so no week is
lost. There is no refuse-first: a run inside a period always
overwrites it, because the second run of every week targets the same
path.

The `<customer-slug>` is the `org::CODE` value lowercased with
punctuation collapsed, plus a short hash of the original code —
`acme-eu-3e6aad`. The hash makes the filename unique: without it
`org::"Acme  EU"` and `org::"acme.eu"` would write the same file, and the
second would publish the first's docbuilder UID.

## Monthly rollups

`summarizer monthly` rolls up a whole calendar month of the weekly
rollups you've already published, into one month-end file per audience.

```bash
summarizer monthly                    # previous calendar month
summarizer monthly --month 2026-09    # a specific month, re-runnable
summarizer monthly --dry-run          # print the prompt, spend nothing
```

Schedule it for the 1st of the month, after the last Sunday-night
weekly run. A month always has a trailing partial week — September
2026 owns W37–W40, and W40 runs to 4 October — and on the 1st that
branch does not exist yet, so **the last three days of every month are
in the weekly reports but in no monthly rollup**. Running it on the
5th instead closes the gap, at the cost of also folding 1–4 October
into September's summary. For a customer-facing document, a missing
week reads better than the wrong one, so the 1st is the default; the
trade is yours to make either way.

It reads the weekly files back out of the **target repo**, not from
local disk and not by re-running the pipeline — September's weekly
rollups already exist as text, so this is one LLM call per audience
rather than a second pass of per-project calls. That also means it
works from any machine with just the reports token, and it rolls up
exactly what was published.

Weeks with no published rollup (the cron didn't run, or the MR was
never pushed) are skipped and listed in the summary rather than
failing the month. A month with no published weeks at all is an error
— an empty month-end summary would be worse than none.

A week is looked for on its source branch first, then on the target's
default branch. GitLab forks a branch from the default branch, so a
merged week's file is at the same path on both — which matters when
"delete source branch on merge" is on, a project setting or a box
ticked in the merge dialog. Without the fallback a fully published,
fully merged month read as an empty one.

Output lands in `reports/<YYYY-MM>/monthly--<YYYY-MM>--<audience>.md`,
on its own branch and MR, so it never collides with a week's.

Per-customer epic rollups (`combined--epics-<customer>--*`) are **not**
folded into the monthly summary. They're already split per customer;
combining them into one blob would mix customers together, which is
what that split exists to prevent.

The monthly path applies the same prompt-injection guard as `run`: its
input is last month's published text, which is itself derived from
GitLab issue and MR bodies. `safety.enabled: true` in the config covers
both subcommands.

## Schedule

```cron
# Sunday 23:00 UTC — the last day of the ISO week, so one run covers
# Monday-to-Sunday and reports on the week it is closing.
0 23 * * 0 cd /path/to/summarizer && ./summarizer run --project group/subgroup/project >> /var/log/summarizer.log 2>&1

# First of each month at 10:00, roll up the month that just closed.
0 10 1 * * cd /path/to/summarizer && ./summarizer monthly >> /var/log/summarizer.log 2>&1
```

## Design

- `internal/events.NormalizedEvent` is the **port boundary**. GitLab JSON never crosses it.
- Free-text GitLab fields are wrapped in `<data>` markers in the prompt. The model is told explicitly never to follow instructions inside `<data>`. (OWASP LLM01:2025 mitigation.)
- The customer report never sees MR/issue body text. The team report gets bodies, capped and `<data>`-wrapped.
- File writes are atomic (`os.Rename` from `.tmp-*`). A crash mid-write leaves the previous good file untouched.
- Output filenames encode the window, and each period gets its own directory, so a re-run overwrites that period in place and never touches another week's.

## Exit codes (cron-friendly)

| Code | Meaning                                     |
|------|---------------------------------------------|
| 0    | Success                                     |
| 2    | Usage error (missing flag, bad subcommand)  |
| 3    | Config error (missing/invalid YAML, bad `--month`) |
| 4    | GitLab fetch failure                        |
| 5    | LLM call failure                            |
| 6    | File write failure (e.g. target exists)     |
| 7    | Missing env var (token / API key)           |
| 8    | Prompt-injection guard rejected a prompt    |
| 1    | Unclassified failure (see stderr)           |

Both subcommands use this table. `summarizer monthly` used to return 4 for
every failure, so an LLM outage or a guard rejection in the monthly path
looked like a GitLab problem to a wrapper branching on these codes.

Code 5 also covers a response that hit the token cap. That is a
deliberate failure, not an outage: the provider stops mid-sentence and
returns the fragment as valid content, so a truncated report would
otherwise be published as a finished one — `public: true`, merged by a
human who sees a normal-looking file. The affected project is named in
the run summary like any other failure.

## Layout

```
summarizer/
├── cmd/summarizer/        # entry point; wires everything
├── internal/
│   ├── config/            # YAML loader + validation
│   ├── events/            # NormalizedEvent — the port boundary
│   ├── gitlab/            # net/http GitLab REST v4 client
│   ├── llm/               # Provider interface + OpenAI + Anthropic
│   ├── policy/            # D5 audience classifier (pure function)
│   ├── prompt/            # embedded templates + render
│   ├── publisher/         # local-disk + GitLab-MR sinks
│   ├── run/               # pipeline orchestrator
│   ├── safety/            # prompt-injection guard port
│   ├── textutil/          # shared string helpers
│   └── triage/            # activity classifier (decide) port
├── examples/
│   └── summarizer.yaml    # sample config
└── README.md
```

## Out of scope (deliberately, for MVP)

- Built-in scheduler / daemon mode.
- Slack / email / wiki delivery.
- DORA metrics / cycle-time analytics.
- OAuth user flows — PAT via env is sufficient.
- Real-time activity streaming.

These are not anti-features; they are deferred until MVP proves out. See git history for the discarded planning scaffolding.