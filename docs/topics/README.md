# Topic icons

`docs/topics/` ships SVG avatars for the two GitLab project topics summarizer recognises.

| Topic | File | Meaning |
|---|---|---|
| `summarize` | [`summarize.svg`](summarize.svg) | Run the project through summarizer; produce both customer-facing and team-facing reports. |
| `summarize-team-only` | [`summarize-team-only.svg`](summarize-team-only.svg) | Run the project; produce only the team-facing report. Customer rollup is suppressed (D5 audience policy). |

## How to use

For each project that joins the run, an operator in the GitLab UI goes to **Project → Settings → Topics**, adds the topic, and uploads the matching SVG as the topic avatar. Both icons are sized for GitLab's 64×64 avatar slot; SVG scales without quality loss to 16×16 (favicon) and 128×128 (large avatar).

## Conventions

- Both icons share the same document motif, palette, and proportions — they read as a pair at any size.
- The `summarize-team-only` variant adds a small padlock badge in the bottom-right corner to signal "internal / team only." That badge is the only visual difference, so the two stay recognisably related.

## Re-export

If you want a single combined file for documentation or a slide deck:

```bash
# Install rsvg-convert once: apt install librsvg2-bin (Debian) or brew install librsvg
rsvg-convert -w 256 -h 256 docs/topics/summarize.svg -o summarize-256.png
rsvg-convert -w 256 -h 256 docs/topics/summarize-team-only.svg -o summarize-team-only-256.png
```

For favicon use:

```bash
rsvg-convert -w 32 -h 32 docs/topics/summarize.svg -o summarize-32.png
```
