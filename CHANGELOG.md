# Changelog

All notable changes to summarizer are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and this
project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- **Epic-level rollup in per-project reports.** When a project's GitLab group has epics (Premium/Ultimate feature), the customer and team prompts surface them as `## Epics active this period` with one H3 per epic, linked issues nested underneath, and an `## Other activity` section for events without an epic link.
  - Activity gate: an epic only counts toward the `maxActiveEpics=20` cap when at least one of its linked issues has `updated_at` within the window. Dormant epics (no issue activity this period) are silently dropped — they don't surface in the report and don't burn the cap.
  - Auto-detected project → group mapping via `GET /projects/:id`. Summarizer reads `namespace.kind` and `namespace.full_path` from the project metadata; user-namespace projects (kind=`user`) bypass the `/epics` fetch with no extra config.
  - Graceful degradation: Free-tier GitLab (no `/epics`), wrong group path (404), or auth issues are all detected. 403/404 → empty epic section with a one-line stderr note; 401 → operator-visible error.
- **`--discover-group` flag for activity-based discovery.** Scan a GitLab group's projects, probe each one for activity (a single cheap MR-list call per project with `per_page=1`), and roll the active ones into the run alongside any explicit `--project` listings. Recursion into subgroups is reserved for a future change; v1 ships single-level only. Composable with `--project`; dedupes by project path (explicit listings take precedence on duplicates).
- **Discovery manifest to stderr.** Per-group scan summary with one line per scanned project (included / excluded with reason), plus a one-line totals. Diagnostic — not a separate file.

### Changed
- `internal/prompt.Render(...)` signature gained a final `[]*gitlab.Epic` parameter. Pass `nil` for projects without epics to keep the existing flat rendering. All call sites and the existing test fixtures updated.

### Internals (epics)
- New `Epic` and `Namespace` types in `internal/gitlab`. `Epic.StartDate` and `Epic.DueDate` use a custom `Date` decoder that accepts both `YYYY-MM-DD` (GitLab's epic schema) and RFC3339.
- `FetchEpics` and `FetchEpicIssues` are paginated like the existing fetchers but carry a soft-degrade contract for 403/404 (Free-tier / wrong group), distinct from the existing `getPage` path. 401s and 5xxs hard-fail (auth/transport).
- New `events.AttachEpics` correlates issue events with their containing epic by IID. Commits and MRs are intentionally NOT annotated (no issue-linkage in this code path).
- New `events.FilterActiveEpics(window)` is the half-open `[since, until]` filter that the pipeline runs before the cap.

### Internals (discovery)
- New `gitlab.ProjectSummary`, `gitlab.FetchGroupProjects(ctx, group, recursive)`, `gitlab.ProbeProjectActivity(ctx, project, since)`. Probe uses `per_page=1` and a soft-degrade contract (404 → inactive; 403 → hard error).
- New `run.Options.DiscoveredGroups` field. `ProjectResult.FromDiscovery` (new bool) flags per-project whether the project came from discovery vs. explicit listing.

## [0.1.0] - 2026-10-20

### Changed
- Report UID is now a UUID v7 minted by [`inful/mdid`](https://github.com/inful/mdid) (was: a hand-rolled 16-char SHA-256-hex truncation). The mint itself is non-deterministic by design (UUID v7 has random bits); cross-run stability comes from read-before-write in the GitLab publisher, which reads the existing file's frontmatter via [`inful/mdfm`](https://github.com/inful/mdfm) and reuses the existing UID on every re-run.
- Report fingerprint now goes through [`inful/mdfp`](https://github.com/inful/mdfp)'s `CalculateFingerprintFromParts` (was: an inline `crypto/sha256` of the body bytes). Output format is unchanged (SHA-256 hex); the swap is for ecosystem consistency with the rest of the user's markdown tooling.

### Added
- Two new direct dependencies: `github.com/inful/mdid` (UID mint), `github.com/inful/mdfp` (fingerprint). Transitive: `github.com/inful/mdfm` (frontmatter parsing in the read-before-write path).
- Initial release. CLI: `summarizer run --project X --since 7d` — fetch GitLab activity, normalise, render prompt, write two Markdown reports (customer + team audiences). Multi-project runs via repeated `--project X --project Y` (also accepts comma-separated `--project X,Y`). Cross-project LLM rollup written to `combined--<duration>d--<audience>.md` when more than one project. `--include-diffs` opt-in, `--dry-run` mode, atomic disk writes with refuse-first policy. Two LLM providers: OpenAI (default) and Anthropic. `--safety-enabled` opt-in: prompt-injection guard via `inful/decide` (default off). GitLab MR publisher: commits rendered reports to a per-period branch with docbuilder-format frontmatter; `public: true` for customer-audience reports; per-period directory layout; one MR per period.
