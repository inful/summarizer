# Operator runbook

This is the operational playbook for summarizer in production.
Anything you might look for at 09:00 on a Monday because the cron
screwed up.

## Top-of-mind: what you see, what it means

The run emits two kinds of output:

| Channel | Content | When |
|---|---|---|
| stdout | render report file paths, token usage, summary counts | every run |
| stderr | diagnostic / warning / error lines | on every run, including success |

The most useful stderr line is the trailing summary:

```
summarizer: ok — 1 project(s), 47 event(s) (47 used), 2 file(s) written (window=this ISO week (since Monday 00:00 UTC, 60h0m0s elapsed))
  → ./summarizer-out/2026-W43/group_subgroup_project--wk--customer.md
  → ./summarizer-out/2026-W43/group_subgroup_project--wk--team.md
  tokens[group/subgroup/project/customer]: prompt=2103 completion=812 total=2915
  tokens[group/subgroup/project/team]:     prompt=2478 completion=901 total=3379
```

All exit code information is the binary's own `$?` (one of the codes
below), which `cron` mails to the operator.

## Exit codes

| Code | Constant | Meaning | When |
|---|---|---|---|
| 0 | `exitOK` | Success. All projects rendered, all files published. | normal path |
| 1 | *(unclassified)* | Pipeline error that didn't match a typed sentinel. | rare; tightened if it happens often |
| 2 | `exitUsage` | Bad CLI args or no `--project`. | operator typo, automation bug |
| 3 | `exitConfig` | YAML parse error, missing required field, validation failed. | typo in `summarizer.yaml` |
| 4 | `exitGitLab` | GitLab API fetch failed (auth, 5xx exhausted retries, network). | token expired, host unreachable, MR list too big |
| 5 | `exitLLM` | LLM call failed (auth, 4xx, 5xx exhausted retries, prompt-render error, safety-verdict rejection). | OPENAI_API_KEY wrong, model id typo, safety guard said injection |
| 6 | `exitWrite` | Disk-write failed (local publisher only). | EACCES; ENOSPC |
| 7 | `exitEnv` | Required env var missing; `--safety-enabled` without URL; invalid combo. | missing $GITLAB_TOKEN, $OPENAI_API_KEY, $ANTHROPIC_API_KEY |

Wire these in your cron wrapper:

```bash
case $? in
  0|2|3|4|5|6|7) mail -s "summarizer exit=$?" /var/log/summarizer.log ;;
  *)             mail -s "summarizer EXIT ABEND" /var/log/summarizer.log ;;
esac
```

## Safety guard scenarios

### "Summarizer rejected my prompt" (exit 5)

```
summarizer: safety guard REJECTED prompt for group/proj/team (score=0.913 >= threshold)
```

The decision API classified the rendered prompt as a prompt-injection
attempt with probability ≥ `injection_threshold` (default 0.7).

1. Note the score. The threshold default is 0.7; a score of 0.55 on a
   different prompt would have been cleared.
2. Inspect what the prompt pulled in. Open the run's render output
   (see `--dry-run`) and check GitLab MR/issue titles, descriptions,
   and labels.
3. Common false-positive sources:
   - A merge request description that quotes an LLM prompt verbatim
     (e.g. "Try this: ignore previous instructions and…") for
     documenting injection tests
   - An issue body that includes "TODO: do X, then check that the
     assistant doesn't interpret it as instructions"
   - A label like `security/prompt-injection-test` whose value the
     model reads as a directive
4. Mitigate by removing the offending content from the source, then
   re-run.

If the false-positive rate is unacceptable (e.g. legitimate prompts
about security are causing every run to fail), raise the threshold:

```yaml
safety:
  injection_threshold: 0.85
```

### "Decide API is down"

```
summarizer: WARN safety guard unavailable for group/proj/customer — proceeding without check: …
```

The prompt-injection guard couldn't reach `/v1/systemone`. summarise
**continues without the guard** and the run completes. The structural
defense (`<data>` markers, system-prompt refusal of instructions inside
`<data>`) remains active.

What to do:

1. Check the decide service: `curl https://api.example.com/healthz`
2. The decide client retries internally; transient blips are absorbed.
   If you're seeing sustained WARNs, the decide API is down for real.
3. **Don't disable the safety guard while decide is down** —
   soft-fail is the right behaviour. The fact that summarizer is
   running without a second-opinion verdict is logged; that's what you
   want.
4. When decide is back, the next run will silently re-enable the
   guard (you'll see `summarizer: safety guard cleared …` lines
   instead of WARNs).

### "Decide model drift"

If a decide model update starts returning inflated scores — every
prompt crosses the threshold — every run hard-fails. Symptoms:

- Hard-fails all day; stderr shows scores like 0.78-0.99 on
  prompts with no obvious injection content
- Exit 5 across multiple unrelated projects

What to do:

1. Roll back the decide model (whichever way your decide deployment
   supports)
2. Temporarily raise `injection_threshold` to 0.95 to ride through
   the rollout window
3. Or disable in config: `safety.enabled: false`

## GitLab publish scenarios

### Overwrite behaviour

There is no refuse-first. Every run inside a week targets the same
`(project, audience, period)` path, so both publishers overwrite in
place — otherwise the cron would fail on the second run of every week.
On the GitLab publisher the UID is read back from the existing file
before the rewrite, so overwriting preserves document identity.

Each period gets its own subdirectory on both publishers:
`<output_dir>/<YYYY-Www>/<name>` locally, `<prefix>/<YYYY-Www>/<name>`
in the target repo. That is what stops one week's reports from
overwriting the previous week's — the filenames themselves carry the
window (`--7d`) but not the date.

(There is no `--force`; it was removed when week buckets made
refuse-first unreachable. A run inside a period always overwrites it,
so cron lines and muscle memory don't break.)

If you want the previous week's content back, read the file out of git
history — the publisher commits each run to the period branch, so every
intermediate version is recoverable.

## A run failed part-way through

A failure on one project no longer aborts the rest. The run logs
`summarizer: WARN <project> failed, continuing: …` per failure, finishes
the remaining projects, and returns a non-zero exit code naming the
first failure. Check the log for the WARN lines to see which projects
were skipped.

Cross-project rollups are **skipped** when any project failed: a
rollup synthesised over a partial project set would read as a complete
week. Per-project reports for the projects that did succeed are still
published.

### "GitLab MR didn't auto-merge"

summarizer opens merge requests; it does **not** auto-merge. Check
the MR listed in the run summary (`→ https://gitlab.example.com/group/reports/-/merge_requests/N`).
Approve and merge manually when you're happy with the content.

A re-run on the same period pushes new commits to the same source
branch while its MR is open.

**If the MR was already merged or closed**, the re-run writes to that
branch and then stops — there is no MR to reopen and the branch will
never merge again, so nothing reaches `<default_branch>`. This is a
logged no-op, not a failure, and the run still exits zero:

```
summarizer: 2026-W43 was already published (period already published:
2026-W43 is merged); group-proj--wk--customer.md was updated on
summarizer/2026-W43 and will not reach main
summarizer: ok — 2 project(s), 47 event(s) (47 used), 2 file(s) written (2 already published this week)
```

If you need those edits to land, open a fresh branch
(`source_branch_prefix`, or wait for the next week) and copy the file
across.

### "Token denied"

```
403 — 403 Forbidden: missing `api` scope on token
```

The PAT lacks the right scope. Two distinct tokens exist:

| Env var | Purpose | Scope needed |
|---|---|---|
| `GITLAB_TOKEN` | source-projects fetch | `read_api` |
| `GITLAB_REPORTS_TOKEN` | target-publish (write) | `api` |

Rotate by changing the env var on the cron agent. The publisher's
`publish.token_env` config field defaults to `GITLAB_REPORTS_TOKEN`;
the safety guard's `safety.decision_token_env` defaults to
`SAFETY_DECISION_TOKEN`.

## Flag reference

Everything on the command line, and where it can also come from. CLI
wins over config in every case.

| Flag | Default | Notes |
|---|---|---|
| `--project P` | — | repeatable or comma-separated. Exact projects; skips discovery. |
| `--discover-active` | **on when nothing else is set** | every project with activity in the window |
| `--discover-group G` | — | repeatable. One group's projects. |
| `--discover-by-topic` | — | instance-wide, projects carrying `summarize*`. |
| `--internal-only-project P` | — | repeatable. Forces team-only. Unions with the YAML block and the `summarize-team-only` topic. |
| `--period-mode` | **removed** | week numbers replaced it; passing it is a usage error. |
| `--dry-run` | off | no LLM call, no write. |
| `--include-diffs` | off | adds per-MR `/changes` bodies to the team prompt. |
| `--max-events N` | 200 | events per project in the prompt; most recent kept. |
| `--max-projects N` | 50 | projects fed to the LLM. Applied after discovery and opt-out, before any completion. Dropped projects are listed on stderr. |
| `--output-dir D` | from config | local publisher only. |
| `--model M` / `--provider P` | from config | `openai` or `anthropic`. |
| `--openai-key` | `$OPENAI_API_KEY` | prefer the env var. |
| `--openai-base-url` | `$OPENAI_BASE_URL`, then config | local Ollama / vLLM / LM Studio. |
| `--anthropic-key` | `$ANTHROPIC_API_KEY` | prefer the env var. |
| `--config P` | `./summarizer.yaml` | |
| `--gitlab-host` | from config | |
| `--gitlab-token` | `$GITLAB_TOKEN` | source-instance token. Prefer the env var. |
| `--safety-enabled` | off | prompt-injection guard. |
| `--safety-url` | from config | decide API base. |
| `--safety-token` | `$SAFETY_DECISION_TOKEN` | |
| `--month M` | previous month | `summarizer monthly` only, `YYYY-MM`. |

## Config playbook

### Disabling safety

```yaml
# summarizer.yaml
safety:
  enabled: false
```

Or one-off: `summarizer run --safety-enabled=false`.

### Disabling publishing

```yaml
# summarizer.yaml — no publish: block, or set enabled: false
```

Or one-off: drop the `publish:` block entirely (no flag for it
currently).

### Changing cadence

Cron drives cadence. Examples:

```cron
# Sunday 23:00 UTC
0 23 * * 0  cd /path/to/summarizer && ./bin/summarizer run --config ./summarizer.yaml >> /var/log/summarizer.log 2>&1
```

**Sunday, not Monday.** The window is the ISO week — Monday 00:00 UTC
to now — and the output directory is that same week. A Sunday 23:00 run
covers 167 hours and reports on the week ending that day. A Monday run
covers three hours and reports on the week that is only starting, so
the six days before it are summarised by nothing. There is no flag to
widen the window; one run per week is the intended cadence.

A daily cron also works and refines each week across seven runs, at
seven times the LLM cost. It costs nothing in correctness, and it is
the better choice if a missed Sunday run would be unacceptable — a
weekly schedule loses the whole week if the machine is off at 23:00.

All of this is UTC. `WeekStart` and the period slug both use UTC, so a
cron that fires in local time will not land where the filename says.
Set `CRON_TZ=UTC` or run the scheduler in UTC.

### Rotating tokens

The two PATs (source fetch + target publish) are operator-managed
secrets. Rotation procedure:

1. Issue a new PAT in GitLab with the same scope on the same account
2. Update the cron agent's env (don't restart the agent — next
   invocation reads the new env from `EnvironmentFile`)
3. Revoke the old PAT

The `summarizer.yaml` file does not carry secrets by design; do **not**
add `token: …` fields to it.

### Debugging with --dry-run

`summarizer run --dry-run` does the GitLab fetch and prints a
one-line summary of how many events would go to the LLM. It does
**not** call the LLM and does **not** write files.

Useful for:

- Confirming a `GITLAB_TOKEN` works end-to-end
- Confirming a project path resolves on your GitLab host
- Spot-checking whether there's any activity in the window before
  paying for LLM tokens
- Reproducing an empty-window condition (no MRs + no issues + no
  commits at all this week → empty report)

### Empty-window conditions

If the project had no activity in the window, the customer and team
reports render as small "no notable activity in this period" stubs.
This is intentional — a missing report confuses readers; an empty
report makes the cron look healthy. If you suspect a missed event, check
which week it landed in — the window is that week, and a Tuesday run
cannot see Monday's later work. Re-run that week's cron line after the
fact, or read the published branch.

## When CI fails

`.github/workflows/ci.yml` runs `gofmt`, `go vet`, `golangci-lint`,
`go test -race`. Reproduce locally:

```bash
go mod tidy
gofmt -l . | grep -v vendor && echo FMT_FAIL || true
go vet ./...
go test -race ./...
```

`golangci-lint v2.13.2` is pinned; install with `go install
github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2`. Don't
bump without a CI run first.

`govulncheck` is the second job. Output lists GO-IDs and call sites.
Mitigate by upgrading the dep (we only have one: `gopkg.in/yaml.v3` and
`github.com/inful/decide`).
