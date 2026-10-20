# Report frontmatter schema

Every report file committed to the GitLab publisher target repo (or any
publisher that uses `internal/publisher/frontmatter.go`) carries a
docbuilder-format YAML frontmatter. This document defines the schema
that summarizer emits, so a docbuilder operator can ingest without
reading Go source.

## Shape

```
---
uid: ab12cd34ef567890
title: "Weekly customer report — group/proj, 2026-10-19"
date: 2026-10-19T09:00:00Z
lastmod: 2026-10-19T09:00:00Z
fingerprint: 7e5d...e8b9
public: true                  # customer-audience reports only
categories:
  - engineering-update
tags:
  - summarizer
  - customer
  - 2026-10-19
  - per-project
  - group/proj
aliases:
  - /_uid/ab12cd34ef567890/
---

(rendered Markdown body)
```

## Keys

| Key | Type | Source | Purpose |
|---|---|---|---|
| `uid` | 16-char hex (8 bytes) | `SHA-256(project\|audience\|period)[:8]` | Stable identity for the report. Same `(project, audience, period)` ⇒ same UID across runs. docbuilder uses this as a content-independent slug. |
| `title` | quoted string | auto-derived | Human-readable title for the index. |
| `date` | RFC3339 timestamp | `time.Now()` at run time | When the period end-date lands (UTC). |
| `lastmod` | RFC3339 timestamp | `time.Now()` at run time | When this version of the file was rendered. Bumped each re-run. |
| `fingerprint` | 64-char SHA-256 hex | `SHA-256(body)` | Content address. Re-run with byte-identical body ⇒ same fingerprint (docbuilder dedupe key); changed body ⇒ new fingerprint. |
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

The per-project tag is dropped on rollups because there's no single
project; subscribers filter by `rollup` instead.

## Title format

| Report kind | Pattern | Example |
|---|---|---|
| per-project | `Weekly <audience> report — <project>, <period>` | `Weekly customer report — group/proj, 2026-10-19` |
| rollup | `Cross-project rollup <audience> report — all subscribed projects, <period>` | `Cross-project rollup team report — all subscribed projects, 2026-10-19` |

`<period>` is the period slug (`YYYY-MM-DD` of the run day in UTC by
default); rollups use the same slug as their constituent per-project
runs.

## UID determinism

The UID is a content-stable hash of `project|audience|period`. Two
invocations of summarizer with the same three inputs (regardless of the
GitLab activity for that project on the day) produce the same UID. This
is intentional: docbuilder dedupes on UID so a re-run overwrites the
existing document instead of creating a new one. If you need a new
document per run, change the period (e.g. move from `YYYY-MM-DD` to
`YYYY-MM-DD-HH`).

## Fingerprint vs UID — why both

| Property | UID | Fingerprint |
|---|---|---|
| Identity (docbuilder slug) | ✓ | — |
| Content address | — | ✓ |
| Stable across re-runs | ✓ | depends on body |
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
