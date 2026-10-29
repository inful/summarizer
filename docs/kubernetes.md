# Running summarizer on Kubernetes

A CronJob that runs `summarizer run` once a week and publishes the
week's reports as a GitLab merge request.

This is the same binary the cron wrapper runs, with the same schedule
and the same exit codes. What changes is where the output goes: **the
local-disk publisher is pointless in a CronJob**, because the pod's
filesystem dies with the pod. So the cluster deployment publishes to
GitLab (`publish.enabled: true`) and the reports live in the target
project, in a branch, waiting for a human to merge them.

> This manifest has not been applied to a live cluster. It is written
> against the published image and the documented CLI, and every field
> below is explained, but nobody has run it on Kubernetes yet. The
> pieces most likely to need adjusting are the resource limits and the
> `timeZone` field, both called out where they appear.

## Before you start

**The image.** `ghcr.io/inful/summarizer` — a distroless image with no
shell and no package manager, running as uid 65532. There is no `sh`
inside it, so nothing that needs a shell (a retry wrapper, a health
check script) can run in the pod. Every argument has to be on the
`args:` list of the container.

**Three tokens**, and they are not interchangeable:

| Token | Scope | Used for |
|---|---|---|
| `GITLAB_TOKEN` | `read_api` on every project being summarised | fetching MRs, issues, commits, epics |
| `GITLAB_REPORTS_TOKEN` | `api` on the **target** project only | committing the report files and opening the MR |
| `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` | the LLM provider | rendering the reports |

`GITLAB_REPORTS_TOKEN` is a separate PAT on purpose: the publishing
identity can be revoked without losing the read side, and vice versa.
If you use the prompt-injection guard (`safety.enabled`) or the
activity classifier (`triage.enabled`), add `SAFETY_DECISION_TOKEN` for
the decision API as well.

## 1. The secrets

Create them with `kubectl`, not with a YAML file in git:

```bash
kubectl -n reports create secret generic summarizer-secrets \
  --from-literal=GITLAB_TOKEN="$GITLAB_TOKEN" \
  --from-literal=GITLAB_REPORTS_TOKEN="$GITLAB_REPORTS_TOKEN" \
  --from-literal=OPENAI_API_KEY="$OPENAI_API_KEY"
```

The namespace has to exist first: `kubectl create namespace reports`.

## 2. The config

A ConfigMap, mounted read-only. The `publish:` block is what makes the
GitLab MR publisher the sink instead of the pod's filesystem:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: summarizer-config
  namespace: reports
data:
  summarizer.yaml: |
    gitlab_host: "https://gitlab.example.com"

    # The reports live in GitLab, not in the pod.
    publish:
      enabled: true
      host: "https://gitlab.example.com"   # defaults to gitlab_host
      token_env: "GITLAB_REPORTS_TOKEN"
      project: "ops/reports"                # the target project
      prefix: "reports"                    # subdirectory in the target repo
      default_branch: "main"               # the MR merges into this
      source_branch_prefix: "summarizer"   # branch = summarizer/<period>

    # Optional: the prompt-injection guard and the activity classifier
    # both talk to the same decision API.
    # safety:
    #   enabled: true
    #   decision_url: "https://api.example.com"
    # triage:
    #   enabled: true
    #   decision_url: "https://api.example.com"

  # Every key is documented in examples/summarizer.yaml and in the
  # README's "Caps" table.
```

Two things about the target project: it is **skipped by discovery**
(it is summarizer's own publishing repo, so summarising it would spend
an LLM call every week describing last week's reports), and the weekly
branch is `summarizer/<period>` — one branch per ISO week, one MR
against `main`.

## 3. The CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: summarizer-weekly
  namespace: reports
spec:
  # Sunday 23:00 UTC. See "The schedule" below for why Sunday.
  schedule: "0 23 * * 0"
  # Only honoured on clusters that support it; see "The schedule".
  timeZone: "UTC"
  # Two runs must never overlap: they would write the same branch and
  # the same period directory.
  concurrencyPolicy: Forbid
  # If the controller was down at 23:00, start it within the hour
  # rather than skipping the week.
  startingDeadlineSeconds: 3600
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      # Kubernetes retries on ANY non-zero exit, and it cannot tell
      # exit 4 (GitLab blip) from exit 3 (config typo) from exit 8
      # (prompt injection). Zero retries keeps that from costing money.
      backoffLimit: 0
      # Longer than the binary's own 6-hour deadline, so the binary
      # ends the run and prints its summary before Kubernetes kills it.
      activeDeadlineSeconds: 25200
      template:
        spec:
          restartPolicy: Never
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            runAsGroup: 65532
            seccompProfile:
              type: RuntimeDefault
          containers:
            - name: summarizer
              image: ghcr.io/inful/summarizer:latest
              args: ["run", "--config", "/etc/summarizer/summarizer.yaml"]
              env:
                - name: GITLAB_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: summarizer-secrets
                      key: GITLAB_TOKEN
                - name: GITLAB_REPORTS_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: summarizer-secrets
                      key: GITLAB_REPORTS_TOKEN
                - name: OPENAI_API_KEY
                  valueFrom:
                    secretKeyRef:
                      name: summarizer-secrets
                      key: OPENAI_API_KEY
              resources:
                requests:
                  cpu: 100m
                  memory: 256Mi
                limits:
                  cpu: "1"
                  memory: 1Gi
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                capabilities:
                  drop: ["ALL"]
              volumeMounts:
                - name: config
                  mountPath: /etc/summarizer
                  readOnly: true
          volumes:
            - name: config
              configMap:
                name: summarizer-config
```

No `--project` and no `--discover-*` flag: with no discovery input at
all, summarizer defaults to `--discover-active`, which runs every
project the token can see that had activity in the window. A project
opts out with a `no-summarize` topic or a `.summaryignore` file at its
repo root. Narrow it with `--discover-group` or `--project` in `args:`
if you want fewer projects than "everything that moved".

### The fields that matter

**`schedule` and `timeZone`.** The window is the ISO week — Monday
00:00 UTC to now — and the report is filed under that same week. A
Sunday 23:00 run covers 167 hours and reports on the week ending that
day. A Monday run covers three hours and reports on the week that is
only starting, so the six days before it are summarised by nothing.
There is no flag to widen the window; one run per week is the intended
cadence.

The schedule is read in the **kube-controller-manager's** timezone, not
yours. `spec.timeZone` sets it per job on clusters that support the
field; on older clusters the field is silently ignored and the
controller's own timezone applies. The dangerous direction is
west-of-UTC: a controller on UTC-5 fires "Sunday 23:00" at Monday
04:00 UTC, which is the three-hour-Monday case above. If you cannot
set `timeZone`, make sure the controller runs in UTC.

**`concurrencyPolicy: Forbid`.** Two overlapping runs would write the
same branch and the same period directory. Forbid is the only safe
value here — `Replace` would kill a running job mid-report.

**`activeDeadlineSeconds: 25200` (7 hours) against the binary's own
6-hour deadline.** The binary bounds a run at six hours and ends it
itself, printing the run summary. Kubernetes' deadline is *longer* on
purpose: if Kubernetes killed the pod at the same moment, the run
would end without a summary and without saying which projects it had
already published. Give it an hour of slack.

**`backoffLimit: 0`.** Kubernetes retries a failed pod, and it cannot
tell exit 4 (GitLab fetch failed — worth retrying) from exit 3 (a typo
in the config — retrying it will fail identically) from exit 8 (the
prompt-injection guard rejected a prompt — retrying it spends the same
LLM budget and gets the same verdict). With no shell in the image
there is no way to make that decision inside the pod, so the safe
default is one attempt and a failed pod to look at. If you would
rather retry the transient failures, put the retry in whatever
schedules your cluster (or run the CronJob against a wrapper image
that has a shell).

**`readOnlyRootFilesystem: true`.** With `publish.enabled: true` the
binary writes nothing: the only code path that creates a file is the
local-disk publisher, and publishing to GitLab skips it entirely. If
you turn publishing off, this pod will fail with a write error unless
you mount a writable volume at `output_dir` — and remember that the
volume dies with the pod, so those reports go with it.

**`resources`.** The memory bound is the GitLab client's response
limit: 16 MB per response, with four merge-request workers fetching
diffs at once. 256 Mi requested and 1 Gi allowed is comfortable for a
normal week; raise the limit if you run `--include-diffs` over
repositories with large refactors, since a 40 MB diff is read into
memory before it is truncated.

### The monthly rollup

A second CronJob, on the 1st of the month, folds the published weekly
rollups into one month-end file per audience:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: summarizer-monthly
  namespace: reports
spec:
  schedule: "0 2 1 * *"
  timeZone: "UTC"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0
      activeDeadlineSeconds: 3600
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: summarizer
              image: ghcr.io/inful/summarizer:latest
              args: ["monthly", "--config", "/etc/summarizer/summarizer.yaml"]
              env:
                - name: GITLAB_REPORTS_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: summarizer-secrets
                      key: GITLAB_REPORTS_TOKEN
                - name: OPENAI_API_KEY
                  valueFrom:
                    secretKeyRef:
                      name: summarizer-secrets
                      key: OPENAI_API_KEY
              volumeMounts:
                - name: config
                  mountPath: /etc/summarizer
                  readOnly: true
          volumes:
            - name: config
              configMap:
                name: summarizer-config
```

It reads the weekly rollups back out of the **target repo**, so it
needs no extra permissions and it does not re-run the pipeline — one
LLM call per audience, over text that already exists. Note what is
*not* in the manifest: `GITLAB_TOKEN`, the source-read token. The
monthly job never fetches a project, so it has no reason to hold a
credential that can read every project summarizer summarises.

**The 1st, or the 5th?** A month always has a trailing partial week:
September 2026 owns W37–W40, and W40 runs to 4 October, so W40's
branch is only created when the weekly job runs on 4 October. A monthly
job on the 1st therefore misses it — **the last three days of every
month are in the weekly reports but in no monthly rollup**. Moving the
monthly job to the 5th closes the gap, at the cost of also folding
1–4 October into September's summary. For a customer-facing document, a
missing week reads better than the wrong one, so the 1st is the
default. The trade is yours to make either way.

## What to look at

```bash
kubectl -n reports get cronjobs
kubectl -n reports get jobs
kubectl -n reports logs job/summarizer-weekly-<timestamp>
```

The run summary goes to stderr, so it is in the pod log:

```
summarizer: ok — 12 project(s), 340 event(s) (318 used), 26 file(s) written
  → reports/2026-W44/group@public--wk--customer.md
  tokens[group/public/customer]: prompt=2478 completion=901 total=3379
```

**Exit codes are the same ones the cron wrapper branches on** — the
full table is in [`runbook.md`](runbook.md). The ones that matter most
here:

| Code | What it means in a CronJob |
|---|---|
| 0 | The week's reports are in the target repo, in a branch, waiting for a human. The MR is not merged yet and summarizer does not wait for it. |
| 3 | The ConfigMap is wrong. The pod logs the config error and exits; the ConfigMap is the thing to fix. |
| 4 | GitLab refused a fetch — an expired `GITLAB_TOKEN`, or the host is unreachable. |
| 5 | The LLM refused — a wrong `OPENAI_API_KEY`, or a model id that does not exist. |
| 7 | A required environment variable is missing — usually a secret that was renamed in the Secret but not in the CronJob. |
| 8 | The prompt-injection guard rejected a prompt. Not an outage: the run stopped because the guard classified a GitLab MR or issue body as an attempt to manipulate the model, and that is worth reading before retrying. |

A failed pod keeps its logs under the job, and `failedJobsHistoryLimit:
3` keeps the last three. The exit code is the pod's `status.containerStatuses[0].state.terminated.exitCode`.

## If you manage the cluster with GitOps

The three objects above are plain Kubernetes manifests with no
summarizer-specific CRDs, so Flux or Argo CD can apply them as they
are. Put them in your cluster repository alongside your other workloads
rather than applying them by hand, and let the usual reconciliation
and drift detection apply. The Secret is the one to keep out of git:
create it with `kubectl create secret` and let it exist as an
imperative object, or have your secret manager own it.

## Known limits of this document

- **Never applied to a live cluster.** Everything here is written
  against the published image and the documented CLI.
- **The resource limits are a guess.** 256 Mi / 1 Gi is sized from the
  GitLab client's 16 MB response cap and its four merge-request
  workers, not from a measured run.
- **No in-pod retry logic**, because the image has no shell. The
  `backoffLimit: 0` default is the consequence.
- **The monthly job's trailing week** is a documented trade-off, not a
  bug — see above.
