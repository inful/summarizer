# Report frontmatter schema

Every report file committed to the GitLab publisher target repo (or any
publisher that uses `internal/publisher/frontmatter.go`) carries a
docbuilder-format YAML frontmatter. This document defines the schema
that summarizer emits, so a docbuilder operator can ingest without
reading Go source.

## Shape

```
---
uid: 0192b6a0-72cd-7abc-8def-0123456789ab
title: "Weekly customer report — group/proj, 2026-W43"
date: 2026-10-19T09:00:00Z
lastmod: 2026-10-19T09:00:00Z
fingerprint: 7e5d...e8b9
public: true                  # customer-audience reports only
categories:
  - engineering-update
tags:
  - summarizer
  - customer
  - 2026-W43
  - per-project
  - group/proj
aliases:
  - /_uid/0192b6a0-72cd-7abc-8def-0123456789ab/
---

(rendered Markdown body)
```

`date` and `lastmod` are the run timestamp, so they move within a
week while `title` and `tags` stay pinned to the week slug — a weekly
report is refined in place across seven runs but keeps one identity.

## Keys

| Key | Type | Source | Purpose |
|---|---|---|---|
| `uid` | UUID v7 (36 chars, 8-4-4-4-12 hex with hyphens) | mint via [`inful/mdid`](https://github.com/inful/mdid) on first publish; reused on every re-run thereafter | Stable identity for the report. Re-runs for the same period re-use the existing file's UID via read-before-write in the GitLab publisher, so docbuilder's cross-project link identity (`/_uid/<uid>/`) survives moves between projects and re-runs on the same day. Two `summarizer run` calls in the same day do **not** mint two UIDs. |
| `title` | quoted string | auto-derived | Human-readable title for the index. |
| `date` | RFC3339 timestamp | `time.Now()` at run time | When the period end-date lands (UTC). |
| `lastmod` | RFC3339 timestamp | `time.Now()` at run time | When this version of the file was rendered. Bumped each re-run. |
| `fingerprint` | SHA-256 hex (64 chars) | computed via [`inful/mdfp`](https://github.com/inful/mdfp)'s `CalculateFingerprintFromParts` over `(frontmatter="", body)` | Content address. Re-run with byte-identical body ⇒ same fingerprint (docbuilder dedupe key); changed body ⇒ new fingerprint. Going through mdfp keeps summarizer hashed the same way as every other markdown file in the ecosystem (mdfm-aware canonical form). |
| `public` | bool (emitted only when true) | pipeline | `true` for customer-audience reports; **absent** for team-audience reports (docbuilder's default is internal). |
| `categories` | list (always one) | fixed `["engineering-update"]` | Top-level document kind. |
| `tags` | list, auto-derived | see below | Filters / searchable labels. |
| `aliases` | list, always one | auto-derived | `/_uid/<uid>/` — docbuilder's stable cross-reference path. |

## Tags per report kind

| Report kind | Tags (always prefix `summarizer, <audience>, <period>, …`) | `public` |
|---|---|---|
| per-project, customer | `summarizer, customer, <period>, per-project, <project>` | `true` |
| per-project, team | `summarizer, team, <period>, per-project, <project>` | *(omitted)* |
| rollup, customer | `summarizer, customer, <period>, rollup` | `true` |
| rollup, team | `summarizer, team, <period>, rollup` | *(omitted)* |
| monthly rollup, customer | `summarizer, customer, <month>, rollup, monthly` | `true` |
| monthly rollup, team | `summarizer, team, <month>, rollup, monthly` | *(omitted)* |

The per-project tag is dropped on rollups because there's no single
project; subscribers filter by `rollup` instead. Monthly rollups carry
`monthly` too, so a subscriber can select "a whole month" and "a single
month" apart without parsing the period slug.

## Title format

| Report kind | Pattern | Example |
|---|---|---|
| per-project | `Weekly <audience> report — <project>, <period>` | `Weekly customer report — group/proj, 2026-W43` |
| rollup | `Cross-project rollup <audience> report — all subscribed projects, <period>` | `Cross-project rollup team report — all subscribed projects, 2026-W43` |
| monthly rollup | `Monthly cross-project rollup <audience> report — all subscribed projects, <month>` | `Monthly cross-project rollup customer report — all subscribed projects, 2026-09` |

`<period>` is the ISO-8601 week slug (`YYYY-Www`) of the run, computed
in UTC. Every run inside the same Mon–Sun week shares one period slug,
so daily runs overwrite each other on purpose rather than accumulating
seven directories a week. Monthly rollups use the month itself
(`YYYY-MM`), which never collides with a weekly slug.

## UID stability — across re-runs and across project moves

`mdid.GenerateUID()` produces a fresh UUID v7 on every call (it's
time-stamped + random — by design, UUID v7 is NOT deterministic).
Summarizer's UID stability across runs and moves therefore comes from
**read-before-write** in the GitLab publisher, not from the mint itself:

1. First publish for a period: `mdid.GenerateUID()` mints a fresh UUID v7.
2. Re-run for the same period: the publisher GETs the existing file from
   the branch via the GitLab Files API, parses its frontmatter with
   [`inful/mdfm`](https://github.com/inful/mdfm), extracts the `uid`
   field, and reuses it verbatim.
3. Move the file between projects in docbuilder: the UID stored in the
   file's frontmatter moves with it. The `aliases: /_uid/<uid>/` line
   keeps the cross-reference path stable.

The **only** way to get a new UID for a `(project, audience, period)`
triple on the GitLab publisher is to delete the existing file from the
branch first. Both publishers overwrite in place otherwise: every run
in a week targets the same `(project, audience, period)` path, so
refuse-first would break the cron from the second run of the week. The
UID survives because it is read back from the existing file before the
new one is written.

**The read happens after the branch is created, on purpose.** GitLab
forks a new branch from the default branch, so a merged week whose
source branch was then deleted — GitLab's "delete source branch on
merge" project setting, or a human ticking that box in the merge
dialog — still has the file and its UID on `main`. Ensuring the branch
first means it carries `main`'s tree and the read finds the real UID.
Reading first would 404, look like a first publish, and mint a fresh
UUID over a document whose identity was live — breaking every
`/_uid/<uid>/` link to it, silently.

## Fingerprint vs UID — why both

| Property | UID | Fingerprint |
|---|---|---|
| Identity (docbuilder cross-project slug) | ✓ | — |
| Content address | — | ✓ |
| Stable across re-runs | ✓ (via read-before-write) | depends on body |
| Captures "this run actually wrote new content" | — | ✓ |

A re-run over the same period with the same activity generates the same
Markdown; `UID` and `fingerprint` are both identical to the prior run.
A re-run with new activity (e.g. the cron fires later on the same day
because the operator is testing) generates different Markdown;
`UID` is the same (docbuilder reuses the same document slot) but
`fingerprint` differs (docbuilder notes the content has changed and
can decide whether to re-publish).

## Local disk output has no frontmatter

The local-disk publisher (`summarizer.yaml` with no `publish:` block)
writes raw Markdown only — no frontmatter, no `---` fences. Frontmatter
emission is exclusively the GitLab publisher's job, because the only
load-bearing reason to have it is to make docbuilder happy when it
ingests the file.

## Output file inventory per run

A complete non-dry-run produces up to 5 distinct file shapes per
period, all under `<prefix>/<period>/`:

| File shape | Audience | When it lands |
|---|---|---|
| `<project-sanitized>--wk--<aud>.md` | customer + team | one pair per project (always) |
| `combined--rollup--wk--<aud>.md` | customer + team | when ≥1 project runs |
| `combined--epics-rollup--wk--team.md` | team only | when ≥1 active epic exists anywhere in the run |
| `combined--epics-<customer-slug>--wk--customer.md` | customer only | one per `org::CODE` bucket |
| `monthly--<YYYY-MM>--<aud>.md` | customer + team | `summarizer monthly` only, under its own `<YYYY-MM>` period directory |

The window token is `wk`, not a day count: the window is the current ISO
week and there is no flag to change it.

The `<customer-slug>` is the `org::CODE` value lowercased and
punctuation collapsed, plus a short hash of the original code —
`acme-eu-3e6aad`. The hash is what makes the filename unique: without
it `org::"Acme  EU"` and `org::"acme.eu"` would write the same file, and
the second would publish the first's docbuilder UID.

There is no customer-side combined epic file. Unattributed epics — no
`org::CODE` label on the epic or on any of its issues — are in the team
rollup only, because customer output is opt-in everywhere else in
summarizer and an unattributed epic is the least attributed thing there
is.

Every `combined--*` file carries its kind as an explicit segment, so a
docbuilder operator browsing the period directory can tell a plain
cross-project rollup from an epic rollup without rendering either. The
two epic shapes differ by whether a customer slug follows `epics-`.

When several discovery inputs are supplied (`--project`,
`--discover-group`, `--discover-by-topic`, `--discover-active`), every
file above covers the union of the projects they select.

