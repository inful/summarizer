# Topic icons

`docs/topics/` ships SVG source and a 192×192 PNG render for each of the three GitLab project topics summarizer recognises.

Topics decide **audience**, and one topic decides **whether the project runs at all**. Any active project runs by default and produces a team-only report.

| Topic | SVG (source) | PNG (192×192) | Meaning |
|---|---|---|---|
| `summarize` | [`summarize.svg`](summarize.svg) | [`summarize-192.png`](summarize-192.png) | Produce a customer-facing report in addition to the team one. The only opt-in to customer output. |
| `summarize-team-only` | [`summarize-team-only.svg`](summarize-team-only.svg) | [`summarize-team-only-192.png`](summarize-team-only-192.png) | Team-only even if the project also carries `summarize`. Redundant with the default; kept so an existing project can pin the intent explicitly. |
| `no-summarize` | [`no-summarize.svg`](no-summarize.svg) | [`no-summarize-192.png`](no-summarize-192.png) | Do not run this project at all. Wins over everything else, including `summarize` and an explicit `--project`. |

## Why there are two ways to opt out

`no-summarize` is an **alternative** to the `.summaryignore` / `.docignore`
file at the repo root, not a replacement. The file is the better signal —
version-controlled, greppable, visible to everyone who clones the repo — but
it needs a commit and a merge, and it puts noise in a project's history for
something that is not a code change.

A topic is a one-click reversible toggle. That is what you want for "noisy
this sprint, skip it for a fortnight", or a repo you administer in GitLab
but should not be pushing branches into.

The cost is visibility: a topic is invisible to anyone reading the
repository. The reason is therefore named in the discovery log, so a skipped
project is still explainable:

```
  excluded (topic no-summarize): group/legacy-billing
  excluded (opt-out file): group/archive
  excluded (publish target): sfb/reports
```

## How to use

For each project that joins the run, an operator in the GitLab UI goes to **Project → Settings → Topics**, adds the topic, and uploads the matching **PNG** file. SVG is the resolution-independent source; PNG at 192×192 is the size GitLab renders for the topic chip. The SVGs are kept in the repo as the version-controlled source of truth.

## Visual language

Both icons share the same document motif, palette (`#dbeafe` panel, `#2563eb` outlines, `#94a3b8` data lines, `#1f2937` lock), and proportions. The `summarize-team-only` variant adds a small padlock badge in the bottom-right corner to signal "internal / team only." That badge is the only visual difference, so the two stay recognisably related at any size.

## Re-rendering the PNGs

The PNGs are checked in so the topic upload is one click, but they're regeneratable from the SVGs if the design changes:

```bash
# Requires librsvg (Debian: apt install librsvg2-bin, macOS: brew install librsvg)
rsvg-convert -w 192 -h 192 docs/topics/summarize.svg           -o docs/topics/summarize-192.png
rsvg-convert -w 192 -h 192 docs/topics/summarize-team-only.svg -o docs/topics/summarize-team-only-192.png
```

To add a different size, change the `-w`/`-h` flags. The SVG renders cleanly at any size.

## Size

Each PNG is well under GitLab's 200 KB topic-avatar limit (about 3–6 KB at 192×192, 1–1.5 KB at 64×64). Each SVG is sub-1.2 KB.

## Why SVG + PNG, not just one

GitLab's topic avatar API accepts both. SVG is the right source-of-truth: resolution-independent, version-control-friendly, and small. PNG at 192×192 is what GitLab's UI actually renders for the topic chip, so a project owner who just wants to upload doesn't have to install `librsvg` to do it.
