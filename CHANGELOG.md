# Changelog

All notable changes to summarizer are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/), and this
project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed
- Report UID is now a UUID v7 minted by [`inful/mdid`](https://github.com/inful/mdid) (was: a hand-rolled 16-char SHA-256-hex truncation). The mint itself is non-deterministic by design (UUID v7 has random bits); cross-run stability comes from read-before-write in the GitLab publisher, which reads the existing file's frontmatter via [`inful/mdfm`](https://github.com/inful/mdfm) and reuses the existing UID on every re-run.
- Report fingerprint now goes through [`inful/mdfp`](https://github.com/inful/mdfp)'s `CalculateFingerprintFromParts` (was: an inline `crypto/sha256` of the body bytes). Output format is unchanged (SHA-256 hex); the swap is for ecosystem consistency with the rest of the user's markdown tooling.

### Added
- Two new direct dependencies: `github.com/inful/mdid` (UID mint), `github.com/inful/mdfp` (fingerprint). Transitive: `github.com/inful/mdfm` (frontmatter parsing in the read-before-write path).

## [0.1.0] - 2026-10-20

### Added
- Initial release.
- CLI: `summarizer run --project X --since 7d` — fetch GitLab activity, normalise, render prompt, write two Markdown reports (customer + team audiences).
- Multi-project runs via repeated `--project X --project Y` (also accepts comma-separated `--project X,Y`).
- Cross-project LLM rollup written to `combined--<duration>d--<audience>.md` when more than one project.
- `--include-diffs` opt-in: per-MR file deltas from `/merge_requests/:iid/changes` flow into the team prompt so the model can reason about code changes rather than just metadata.
- `--dry-run` mode: fetch GitLab data, print event count summary, exit. No LLM call, no file write.
- Atomic disk writes (`tmp` + `fsync` + `rename`); refuse-first policy unless `--force`.
- Local-first defaults; OpenAI-compatible local servers (Ollama, vLLM, LM Studio) via `--openai-base-url`.
- Two LLM providers out of the box: OpenAI (default) and Anthropic. Pluggable `Provider` interface.
- `--safety-enabled` opt-in: prompt-injection guard via [`inful/decide`](https://github.com/inful/decide) (Go client for `/v1/systemone`). Soft-fail on guard errors (decision API down), hard-fail on `VerdictInjection`.
- `--publish-enabled`-style output: commit reports to a GitLab project as merge requests for review instead of writing locally. Docbuilder-format frontmatter; `public: true` for customer-audience reports so docbuilder exposes them externally; per-period directory layout (`<prefix>/<period>/<filename>`); one MR per period so re-runs accumulate on the same MR.
- Distinct GitLab tokens for source fetch (`GITLAB_TOKEN`) and target publish (`GITLAB_REPORTS_TOKEN`).
- Self-hosted GitLab support via the `GitLabHost` config (auto-prepends `/api/v4` to web-UI bases; accepts both bare-host and `/v1`-suffixed forms for OpenAI-compatible endpoints).
- Exit codes 0..7 (success, usage, config, GitLab, LLM, write, env).
- `signal.NotifyContext` so SIGINT/SIGTERM cancels in-flight LLM and GitLab calls cleanly.

### Security
- Per-audience content sanitisation: customer prompt never sees MR/issue bodies; team prompt receives them wrapped in `<data>` markers; the team system prompt explicitly forbids obeying instructions found inside `<data>`.
- Token / credential handling: secrets only in env vars; `summarizer.yaml` rejects commits containing `token:` values.
- Atomic file writes leave no half-written files on crash.

### Known limitations
- No discussion / comment-body fetching (only count).
- No epic / iteration / milestone grouping yet.
- No project auto-enrollment; subscription lists are operator-curated.
