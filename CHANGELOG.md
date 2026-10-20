# Changelog

All notable changes to summarizer are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and this
project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- **`--discover-by-topic` — server-wide discovery.** Picks up every project on the GitLab instance carrying a `summarize` or `summarize-team-only` topic, across all groups the token can see, with no group enumeration. GitLab's `?topic=` filter is AND-shaped, so the client issues one `GET /projects?topic=X` per marker and unions by project ID; a project with both topics is fetched once and keeps its full topic list. Same MR-activity gate, same D5 audience classification and same `(team-only)` manifest annotation as `--discover-group`. Composes with `--project` (explicit wins, not marked discovered) and `--discover-group` (union). Bounded by the PAT's visibility — a token that can't see a project won't return it.
- **Fixed: `--discover-group` alone exited with `at least one --project is required`.** The CLI gate checked only `--project`, contradicting both the README and the pipeline's own gate. The gate now accepts `--project`, `--discover-group` or `--discover-by-topic`.
- **`--period-mode daily|monday`.** Controls the output bucket shape. `daily` (default, v0.1.0 behaviour) gives each cron tick its own `<prefix>/<YYYY-MM-DD>/` directory, branch and MR. `monday` anchors the bucket to the ISO week's Monday, so daily runs across Mon–Sun overwrite the same directory; the GitLab publisher accumulates commits on one branch and reuses the open MR, and the local-disk publisher implies `Force` so the cron doesn't start failing on day 2. Crossing into a new week opens a new bucket. Unknown values fail at parse time (case-sensitive; `weekly` is not accepted — the bucket is a date, not an ISO week number).
- **D5 — GitLab-as-control-plane audience policy** (D5 from `docs/deferred.md`). Three sources of "is this project customer-eligible" union together: the YAML `internal_only_projects:` block, the `--internal-only-project` CLI flag, and the GitLab project's topics (`summarize` for opt-in to customer output; `summarize-team-only` for the override). Discovered projects (via `--discover-group`) default to team-only unless `summarize` is set — the default-safe behavior keeps the customer side bounded even when the operator doesn't know what's in a group.
- **Cross-project epic rollup file** (`combined--epics--<period>--<audience>.md`). Fires whenever ≥1 active epic exists anywhere in the run, independent of project count. Each project's active epics land in dedicated `# Project: …` sections with title, state, web URL, and event count; the LLM synthesizes cross-project themes for both customer and team audiences. One extra LLM call per audience per run.
- **Epic-level rollup in per-project reports.** When a project's GitLab group has epics (Premium/Ultimate feature), the customer and team prompts surface them as `## Epics active this period` with one H3 per epic, linked issues nested underneath, and an `## Other activity` section for events without an epic link.
  - Activity gate: an epic only counts toward the `maxActiveEpics=20` cap when at least one of its linked issues has `updated_at` within the window. Dormant epics (no issue activity this period) are silently dropped — they don't surface in the report and don't burn the cap.
  - Auto-detected project → group mapping via `GET /projects/:id`. Summarizer reads `namespace.kind` and `namespace.full_path` from the project metadata; user-namespace projects (kind=`user`) bypass the `/epics` fetch with no extra config.
  - Graceful degradation: Free-tier GitLab (no `/epics`), wrong group path (404), or auth issues are all detected. 403/404 → empty epic section with a one-line stderr note; 401 → operator-visible error.
- **`--discover-group` flag for activity-based discovery.** Scan a GitLab group's projects, probe each one for activity (a single cheap MR-list call per project with `per_page=1`), and roll the active ones into the run alongside any explicit `--project` listings. Recursion into subgroups is reserved for a future change; v1 ships single-level only. Composable with `--project`; dedupes by project path (explicit listings take precedence on duplicates).
- **Discovery manifest to stderr.** Per-group scan summary with one line per scanned project (included / excluded with reason), plus a one-line totals. Diagnostic — not a separate file.

### Changed
- `internal/prompt.Render(...)` signature gained a final `[]*gitlab.Epic` parameter. Pass `nil` for projects without epics to keep the existing flat rendering. All call sites and the existing test fixtures updated.
- `internal/run.ProjectResult` gained `Source`, `TeamOnly`, `CustomerOn` fields carrying the D5 audience policy forward so rollup filters don't recompute.

### Internals (epics)
- New `Epic` and `Namespace` types in `internal/gitlab`. `Epic.StartDate` and `Epic.DueDate` use a custom `Date` decoder that accepts both `YYYY-MM-DD` (GitLab's epic schema) and RFC3339.
- `FetchEpics` and `FetchEpicIssues` are paginated like the existing fetchers but carry a soft-degrade contract for 403/404 (Free-tier / wrong group), distinct from the existing `getPage` path. 401s and 5xxs hard-fail (auth/transport).
- New `events.AttachEpics` correlates issue events with their containing epic by IID. Commits and MRs are intentionally NOT annotated (no issue-linkage in this code path).
- New `events.FilterActiveEpics(window)` is the half-open `[since, until]` filter that the pipeline runs before the cap.

### Internals (discovery)
- New `gitlab.ProjectSummary`, `gitlab.FetchGroupProjects(ctx, group, recursive)`, `gitlab.ProbeProjectActivity(ctx, project, since)`. Probe uses `per_page=1` and a soft-degrade contract (404 → inactive; 403 → hard error). `ProjectSummary.Topics` carries the project's GitLab topic list (used by D5 audience policy).
- New `gitlab.FetchProjectSummary(ctx, projectPath)` — single-project `/projects/:id` fetch used by the audience-policy classifier. Soft-degrades 404 → (nil, nil).
- New `run.Options.DiscoveredGroups` field. `ProjectResult.FromDiscovery` (new bool) flags per-project whether the project came from discovery vs. explicit listing.

### Internals (D5 / audience policy)
- New `internal/policy` package — `ProjectSource` enum (`SourceExplicit`, `SourceDiscovered`), `ProjectPolicy`, and a pure-function `Classify(source, topics, inList)`. The 13-row truth table is the project's truth-table test (`policy_test.go`). Topics are case-sensitive exact-token matches (`"summarize-team-only"` is a distinct label from `"Summarize-Team-Only"`).
- Discovery manifest annotates each scanned project with a `(team-only)` tag when the classifier says so, so the operator's stderr audit trail tells them what's classified how.

## [0.1.0] - 2026-10-20

### Changed
- Report UID is now a UUID v7 minted by [`inful/mdid`](https://github.com/inful/mdid) (was: a hand-rolled 16-char SHA-256-hex truncation). The mint itself is non-deterministic by design (UUID v7 has random bits); cross-run stability comes from read-before-write in the GitLab publisher, which reads the existing file's frontmatter via [`inful/mdfm`](https://github.com/inful/mdfm) and reuses the existing UID on every re-run.
- Report fingerprint now goes through [`inful/mdfp`](https://github.com/inful/mdfp)'s `CalculateFingerprintFromParts` (was: an inline `crypto/sha256` of the body bytes). Output format is unchanged (SHA-256 hex); the swap is for ecosystem consistency with the rest of the user's markdown tooling.

### Added
- Two new direct dependencies: `github.com/inful/mdid` (UID mint), `github.com/inful/mdfp` (fingerprint). Transitive: `github.com/inful/mdfm` (frontmatter parsing in the read-before-write path).
- Initial release. CLI: `summarizer run --project X --since 7d` — fetch GitLab activity, normalise, render prompt, write two Markdown reports (customer + team audiences). Multi-project runs via repeated `--project X --project Y` (also accepts comma-separated `--project X,Y`). Cross-project LLM rollup written to `combined--<duration>d--<audience>.md` when more than one project. `--include-diffs` opt-in, `--dry-run` mode, atomic disk writes with refuse-first policy. Two LLM providers: OpenAI (default) and Anthropic. `--safety-enabled` opt-in: prompt-injection guard via `inful/decide` (default off). GitLab MR publisher: commits rendered reports to a per-period branch with docbuilder-format frontmatter; `public: true` for customer-audience reports; per-period directory layout; one MR per period.
