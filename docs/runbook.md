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
summarizer: ok — 1 project(s), 47 event(s) (47 used), 2 file(s) written (window=168h0m0s)
  → ./summarizer-out/group_subgroup_project--7d--customer.md
  → ./summarizer-out/group_subgroup_project--7d--team.md
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
| 6 | `exitWrite` | Refused to overwrite an existing output file; or disk-write failed. | `--force` missing; EACCES; ENOSPC |
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

### "Refuse overwrite" (exit 6)

```
publisher: target file already exists (use --force to overwrite)
```

The local-disk publisher refuses to overwrite an existing file unless
`--force` is passed. For re-running the same window deliberately, use
`--force`. For accidental double-runs, the file from the first run is
preserved (no data loss).

### "GitLab MR didn't auto-merge"

summarizer opens merge requests; it does **not** auto-merge. Check
the MR listed in the run summary (`→ https://gitlab.example.com/group/reports/-/merge_requests/N`).
Approve and merge manually when you're happy with the content.

A re-run on the same period pushes new commits to the same source
branch — if the MR was already merged, summarizer **refuses to reopen**
and exits non-zero. Open a new branch (change `source_branch_prefix`
or wait for a new date) if you want to re-publish merged content.

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
# Weekly Monday morning, 09:00 UTC
0 9 * * 1  cd /path/to/summarizer && ./bin/summarizer run --config ./summarizer.yaml >> /var/log/summarizer.log 2>&1

# Daily for a busy team
0 9 * * *  cd /path/to/summarizer && ./bin/summarizer run --config ./summarizer.yaml --since 1d >> /var/log/summarizer.log 2>&1
```

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
  commits in 7 days → empty report)

### Empty-window conditions

If the project had no activity in the window, the customer and team
reports render as small "no notable activity in this period" stubs.
This is intentional — a missing report confuses readers; an empty
report makes the cron look healthy. Use `--since` to widen the
window if you suspect a missed event.

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
