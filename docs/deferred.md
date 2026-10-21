# Deferred features

A canonical reference for everything summarizer doesn't do yet. Anything
listed here is either consciously not built or consciously left out of
scope. If you're picking up this project and wondering "why doesn't
summarizer do X?" — look here first.

For what summarizer *does* do today, see `README.md` and `docs/how-it-works.md`.

---

## Not yet built

Features the operator has flagged but the implementation hasn't landed.
Each entry names the gap, the rough shape of the change, and any
prior conversation or constraint that drove the deferral.

### Deployment-shape (operator's "open questions" sketch)

| Item | What's missing | Rough shape of the change |
|---|---|---|
| **D1 — `groups` config + `--group` flag** | A `groups:` block in `summarizer.yaml` that resolves a GitLab group path into its descendant projects at run time, plus a `--group` flag mirroring `--project`. | Pure config plumbing in `internal/config` + flag/parse extension in `cmd/summarizer`. Small. |
| ~~**D2 — Epic-level rollup**~~ | shipped (see CHANGELOG). Epics now surface in the per-project report when Premium/Ultimate epics exist; shape A (events grouped under epic headers in the existing file). User-namespace projects, Free-tier GitLab (no epics), and 403s degrade silently with a one-line stderr note. | The four shapes discussed originally: A was chosen as the smallest viable. B/C/D remain on the table if the operator later wants dedicated epic files, a separate `--epics-only` mode, or an epic-first design that depends on D1/D3. |
| **D3 — `discover` mode (label-based)** | A new subcommand (likely `summarizer discover`) that walks a top-level group, inspects each project's labels for an opt-in marker, and emits a candidate `summarizer.yaml`. Distinct from the shipped activity-based discovery below: D3 selects via GitLab-side labels (operator decides enrollment); the shipped version selects via activity (no operator maintenance, but jittery on a single quiet day). | New endpoint surface in `internal/gitlab`; new command in `cmd/summarizer`; output is config, not reports. |
| **D4 — Subscription + cadence config** | Subscription lists and run cadences are pure-config today (`summarizer.yaml` + cron). No awareness in the binary of "this project runs weekly, that one runs monthly." | New config schema; mostly additive — pipeline today is cron-driven by design. |
| ~~**D5 — GitLab-as-control-plane enrollment**~~ | shipped, then inverted (see CHANGELOG). Every active project now runs by default; topics on GitLab (`summarize` for opt-in to customer output, `summarize-team-only` as a redundant explicit override) decide audience only, in concert with the YAML `internal_only_projects:` block and the `--internal-only-project` CLI flag. Whether a project runs at all is decided by the `no-summarize` topic (no commit needed) or a `.summaryignore` / `.docignore` file at its repo root; the reports project is excluded automatically. | Labeling every project was the one-time enrollment cost the inversion removed, so this is no longer the go-live plan. Grouping rollups through GitLab (the original D5 mention) is still future work. |
| ~~**Activity-based discovery**~~ | shipped, then made the default. Discovery now runs every project the token can see that had activity in the window (`FetchActiveProjects`, via GitLab's `last_activity_after` filter) with no flag at all. The activity gate (`ProbeProjectActivity`) falls through MRs → issues → commits, so a repo maintained by direct pushes to `main` still counts as active. `--discover-group` remains as a narrower scope; recursion into subgroups is still reserved for v2 — see `gitlab.FetchGroupProjects(ctx, group, recursive)`, which errors when `recursive=true`. | Distinct from D3 (label-based enrollment): activity-based needs no operator-maintained label, but is jittery on a one-off quiet day (a project with no activity in the window simply doesn't appear). |

### Safety guard extensions

The safety guard (`--safety-enabled`) was built as a single-shot check.
These extend it.

| Item | What's missing | Why deferred |
|---|---|---|
| **S1 — Post-generation checking** | We gate the **prompt** before sending it to the LLM. We do not check the model's **output** for new injection that the model itself produced. | Output-side validation is a separate concern (output content policy, not injection). Add later via an output-guard interface alongside `safety.Guard`. |
| **S2 — Score telemetry / metrics** | Every safe verdict logs `score=0.123` to stderr, but it's unstructured. | Promote to a metric (file, JSON, or statsd) once there's a destination. |
| **S3 — Decision-result cache** | Two adjacent LLM calls (customer + team) for the same project produce near-identical prompts; today the guard re-decides each time. | Could hash-and-cache the prompt + decision. ~2x latency hit is small enough that it was deferred. |
| **S4 — Configurable hard/soft-fail on guard errors** | Hardcoded soft-fail: a flaky `/v1/systemone` does not block summary generation. The "hard-fail on transport error" alternative is not exposed as a flag. | Operator chose soft-fail; a `safety.fail_closed_on_error bool` could be added if a security-sensitive deployment wants the opposite. |
| **S5 — `--i-know-what-im-doing` override on injection verdict** | A `VerdictInjection` verdict is final; no override path. | Intentionally not added — would defeat the guard's purpose. If you need this, add it explicitly with a clear "I am overriding a security check" log line. |

### Production validation

Two paths the project's own code covers with httptest but have never
been walked end-to-end with real credentials:

| Item | What's missing |
|---|---|
| **P1 — End-to-end against real GitLab** | httptest covers the publisher's wire shape; a live PAT against a write-capable GitLab target project is the one path nobody has walked. |
| **P2 — End-to-end against real `/v1/systemone`** | Same caveat for the safety guard's decision API. |

Both are 30-minute affairs once a token is issued. Cheap to do, just
not done.

### Code quality

| Item | What's missing | Why deferred |
|---|---|---|
| **Q1 — Typed-error sentinels for exit codes** | Partially done. `classify` matches `run.ErrInjection` and `publisher.ErrExists` with `errors.Is`, but GitLab / LLM / render / config failures still fall through to a substring pass over `err.Error()`. | No exit-code mis-routing observed in practice; the string-match is a working stopgap. Finishing it means every error site in `internal/gitlab`, `internal/llm` and `internal/run` wraps a typed sentinel instead of `fmt.Errorf`. |
| ~~**Q2 — `internal/run` pipeline tests**~~ | Shipped. `internal/run` now carries 11 test files: `sourcePipeline` + `recordingLLM` exercise `runOneProject` end-to-end, `capturePublisher` asserts what was published, `countingLLM` pins LLM call counts, and `fakeGitLab` covers discovery, the activity gate, the opt-out file, and the monthly runner against a fake target repo. | Closed. |

---

## Explicit no — out of scope

These are deliberate, not deferred. The maintainer has decided not to
build them. If a contributor proposes one, the answer is "by design."

- **Real-time activity streaming** — summarizer polls; it does not subscribe to GitLab webhooks for live updates.
- **Operator UI / control plane** — everything is YAML config + cron. No web UI, no admin page.
- **Multi-tenant isolation** — one binary, one operator, one vault. Don't run a shared deployment.
- **Webhook-driven runs** — cron is the scheduler. Async guard is `--safety-enabled`, nothing else.
- **Automatic enrollment** — the subscription list is operator-curated (with D3 on the way to make curation easier).
- **Daemon mode** — the original spec called for a one-shot CLI; long-running mode was always explicit later.
- **Discussion / comment-body fetching** — only counts, not bodies (CHANGELOG "Known limitations"). The prompt-injection attack surface is why; bodies flow through the team prompt as `<data>`-wrapped strings today.
- **README translations** — English only.

---

## Decided against (recorded so we don't re-litigate)

Decisions taken in the project's early history that were deliberate
non-builds. These sometimes have a hidden implementation lurking in
old commit messages; this is where you go to find the rationale.

| Item | What we said "no" to | Why | Where the rationale lives |
|---|---|---|---|
| **Q3 — Migration story for legacy SHA-256-hex UIDs** | A read-before-write branch in the publisher that detects pre-UUID-v7 UIDs and migrates them on first contact. | The project has never been deployed to production, so there are no legacy files in the wild to migrate. If legacy files ever show up, three lines + one test bring it back. | Commit `2bd132b` message; the read-before-write behavior at `internal/publisher/gitlab.go:readExistingUID` already does the right thing (mint fresh on absent uid; reuse verbatim otherwise), so even accidental legacy files self-recover. |
| **S4 — Hard-fail on safety-guard errors** | The first design of the safety guard hard-failed on transport errors (e.g. `/v1/systemone` temporarily unreachable). | Operator chose soft-fail: a flaky decide API shouldn't block summary generation; the structural defense (`<data>` markers + system-prompt refusal rules) is still active when the guard is unavailable. | Commit `4d463d2`. |

---

## Updating this file

When something here is built, move it to a CHANGELOG entry and delete
the row. When the design stance on an "Explicit no" item changes, move
it to "Not yet built" or to CHANGELOG; don't leave a "we used to
consider this out of scope, now we're considering it" half-state.
