package publisher

import (
	"fmt"
	"strings"
	"time"

	"github.com/inful/mdfp"
	"github.com/inful/mdid"
)

// Frontmatter represents the docbuilder-format YAML block that wraps each
// rendered Markdown report. Field names and shape match the docbuilder
// parser in inful/docbuilder (uid, title, date, lastmod, fingerprint,
// tags, categories, aliases) so the rendered reports ingest cleanly into
// any docbuilder-compatible knowledge base.
//
// UID format: UUID v7 minted by github.com/inful/mdid. docbuilder's UID
// is the cross-project identifier that lets a document be moved
// between repos / projects without breaking links. The UID lives in
// the file's own frontmatter (mdid's "won't overwrite" property), so
// cross-run stability comes from read-before-write in the GitLab
// publisher, not from determinism of the mint itself.
type Frontmatter struct {
	UID         string
	Title       string
	Date        time.Time
	LastMod     time.Time
	Fingerprint string
	Tags        []string
	Categories  []string
	Aliases     []string

	// Public controls docbuilder's external publication flag. When true,
	// docbuilder exposes the document beyond the team's internal index;
	// when false (default), the document is internal-only. Summarizer
	// sets this to true only for customer-audience reports so the team
	// reports never bleed into customer-visible surfaces by mistake.
	Public bool
}

// Render produces the YAML frontmatter block (including the --- fences)
// followed by the body. The body is passed through verbatim — no
// postprocessing — so the publisher is the only thing that touches the
// rendered Markdown.
func (f Frontmatter) Render(body []byte) []byte {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("uid: %s\n", f.UID))
	sb.WriteString(fmt.Sprintf("title: %q\n", f.Title))
	sb.WriteString(fmt.Sprintf("date: %s\n", f.Date.UTC().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("lastmod: %s\n", f.LastMod.UTC().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("fingerprint: %s\n", f.Fingerprint))
	if f.Public {
		// Only emit when true; absence implies false (docbuilder's
		// default). Emitting "public: false" would be redundant.
		sb.WriteString("public: true\n")
	}
	if len(f.Categories) > 0 {
		sb.WriteString("categories:\n")
		for _, c := range f.Categories {
			sb.WriteString(fmt.Sprintf("  - %s\n", c))
		}
	}
	if len(f.Tags) > 0 {
		sb.WriteString("tags:\n")
		for _, t := range f.Tags {
			sb.WriteString(fmt.Sprintf("  - %s\n", t))
		}
	}
	if len(f.Aliases) > 0 {
		sb.WriteString("aliases:\n")
		for _, a := range f.Aliases {
			sb.WriteString(fmt.Sprintf("  - %s\n", a))
		}
	}
	sb.WriteString("---\n\n")
	sb.Write(body)
	return []byte(sb.String())
}

// NewFrontmatter builds a docbuilder-format Frontmatter for a report.
//
//   - project:  GitLab project path ("group/proj")
//   - audience: "customer" or "team"
//   - period:   human-readable period label like "2026-10-19" or "2026-W42"
//   - kind:     per-project or rollup
//   - uid:      pre-existing docbuilder UID to reuse, or "" to mint fresh.
//
// When uid is empty, a fresh UUID v7 is minted via inful/mdid. When uid is
// non-empty, it's used verbatim. The publisher reads the existing file
// (when present) and passes the existing uid here so re-runs preserve
// cross-project identity (docbuilder invariant: moving a document between
// projects must not break links).
//
// Fingerprint is a body-content hash via inful/mdfp. Same SHA-256 hex
// shape we'd compute by hand, but through the canonical library so the
// normalisation (BOM stripping, trailing-whitespace handling,
// fingerprint-strip-from-frontmatter logic if we ever switch to wrapping
// before hashing) stays aligned with everything else in the ecosystem.
func NewFrontmatter(project, audience, period string, kind FileKind, now time.Time, body []byte, uid string) Frontmatter {
	if uid == "" {
		uid = mdid.GenerateUID()
	}

	// mdfp.CalculateFingerprintFromParts("", body) hashes the body only —
	// the same SHA-256-of-bytes we'd compute inline, but routed through
	// the canonical library. When we later add a frontmatter-embedding
	// variant (compute fingerprint of the full wrapped file), swapping to
	// mdfp's three-arg form costs one line.
	fingerprint := mdfp.CalculateFingerprintFromParts("", string(body))

	tags := []string{"summarizer", string(audience), period}
	if kind == KindRollup {
		tags = append(tags, "rollup")
	} else {
		tags = append(tags, "per-project")
	}
	if project != "" {
		// Inline the project as a tag too — useful when the docbuilder
		// index is searched by tag.
		tags = append(tags, project)
	}

	titlePrefix := "Weekly"
	switch kind {
	case KindRollup:
		titlePrefix = "Cross-project rollup"
	}
	projectLabel := project
	if kind == KindRollup {
		projectLabel = "all subscribed projects"
	}
	title := fmt.Sprintf("%s %s report — %s, %s", titlePrefix, audience, projectLabel, period)

	return Frontmatter{
		UID:         uid,
		Title:       title,
		Date:        now,
		LastMod:     now,
		Fingerprint: fingerprint,
		Tags:        tags,
		Categories:  []string{"engineering-update"},
		Aliases:     []string{"/_uid/" + uid + "/"},
	}
}
