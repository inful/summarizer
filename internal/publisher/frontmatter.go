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
	fmt.Fprintf(&sb, "uid: %s\n", f.UID)
	fmt.Fprintf(&sb, "title: %q\n", f.Title)
	fmt.Fprintf(&sb, "date: %s\n", f.Date.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "lastmod: %s\n", f.LastMod.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "fingerprint: %s\n", f.Fingerprint)
	if f.Public {
		// Only emit when true; absence implies false (docbuilder's
		// default). Emitting "public: false" would be redundant.
		sb.WriteString("public: true\n")
	}
	if len(f.Categories) > 0 {
		sb.WriteString("categories:\n")
		for _, c := range f.Categories {
			fmt.Fprintf(&sb, "  - %s\n", c)
		}
	}
	if len(f.Tags) > 0 {
		sb.WriteString("tags:\n")
		for _, t := range f.Tags {
			fmt.Fprintf(&sb, "  - %s\n", t)
		}
	}
	if len(f.Aliases) > 0 {
		sb.WriteString("aliases:\n")
		for _, a := range f.Aliases {
			fmt.Fprintf(&sb, "  - %s\n", a)
		}
	}
	sb.WriteString("---\n\n")
	// Stripped here as well as in NewFrontmatter, and that is deliberate:
	// this is the function that assembles the bytes that land in the
	// file, and Publish passes it req.Body rather than the slice
	// NewFrontmatter derived its fingerprint from. Stripping in one
	// place only left the H1 in the document and out of the hash.
	// stripLeadingTitle is idempotent, so the second call is a no-op on
	// an already-stripped body.
	sb.Write(stripLeadingTitle(body))
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

	// Before the fingerprint, not after: the hash has to cover the
	// bytes that actually land in the file, or docbuilder sees a
	// content change on every read.
	//
	// The templates already tell the model not to write a title. This
	// is the enforcement, because a model that ignores the rule would
	// otherwise put a second "# Weekly engineering update" under the
	// frontmatter title that is already there.
	body = stripLeadingTitle(body)

	// mdfp.CalculateFingerprintFromParts("", body) hashes the body only —
	// the same SHA-256-of-bytes we'd compute inline, but routed through
	// the canonical library. When we later add a frontmatter-embedding
	// variant (compute fingerprint of the full wrapped file), swapping to
	// mdfp's three-arg form costs one line.
	fingerprint := mdfp.CalculateFingerprintFromParts("", string(body))

	tags := []string{"summarizer", string(audience), period}
	switch kind {
	case KindRollup:
		tags = append(tags, "rollup")
	case KindMonthly:
		tags = append(tags, "rollup", "monthly")
	default:
		tags = append(tags, "per-project")
	}
	if project != "" {
		// Inline the project as a tag too — useful when the docbuilder
		// index is searched by tag.
		tags = append(tags, project)
	}

	titlePrefix := "Weekly"
	projectLabel := project
	switch kind {
	case KindRollup:
		titlePrefix = "Cross-project rollup"
	case KindMonthly:
		titlePrefix = "Monthly cross-project rollup"
	}
	if projectLabel == "" && (kind == KindRollup || kind == KindMonthly) {
		// A rollup that spans the fleet says so. One that does NOT is
		// per-customer (the epic rollups carry the org:: code in
		// Request.Project), and forcing the fleet label onto it gave
		// every customer's public document the same title and left
		// nothing in the index but the filename to tell them apart.
		//
		// "All" is only true for the team audience. The customer rollup
		// is built from eligibleProjects, which drops every team-only
		// project — customer output is opt-in per project, so on a
		// typical fleet the customer file covers a minority of the run.
		// Measured: 3 projects in the run, 2 in the customer rollup,
		// title reading "…all subscribed projects". The body header is
		// accurate (ProjectCount counts the eligible slice); only the
		// index entry overstated, which is the part a reader sees.
		projectLabel = "all projects"
		// audience is a plain string here — NewFrontmatter takes no
		// prompt.Audience, and this package must not import internal/prompt.
		// It is the same value the title already interpolates.
		if audience == "customer" {
			projectLabel = "all customer-facing projects"
		}
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

// stripLeadingTitle removes a single leading Markdown H1 and the blank
// lines around it, so the body does not repeat the title docbuilder
// already renders from the frontmatter.
//
// Returns body byte-for-byte unchanged when there is no leading H1,
// which keeps the fingerprint stable for every existing document.
//
// Only the GitLab publisher calls this, and that is correct: the
// local-disk publisher writes no frontmatter, so on disk the H1 is the
// only title the file has and removing it would leave an unnamed
// document.
//
// Only one H1, and only at the very start. A "## Highlights" further
// down is content, not a title, and "#not-a-heading" is not a heading
// in Markdown at all — stripping it would corrupt a line the model
// wrote on purpose.
func stripLeadingTitle(body []byte) []byte {
	rest := string(body)
	for {
		line, after, more := strings.Cut(rest, "\n")
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" {
			if !more {
				return body // nothing but blank lines
			}
			rest = after
			continue
		}
		// ATX heading: up to three leading spaces, then "#", then a
		// space, tab or end of line. Four spaces is an indented code
		// block, not a heading.
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		if indent <= 3 && isH1(trimmed[indent:]) {
			return []byte(strings.TrimLeft(after, "\r\n"))
		}
		return body
	}
}

func isH1(line string) bool {
	if !strings.HasPrefix(line, "#") {
		return false
	}
	rest := line[1:]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}
