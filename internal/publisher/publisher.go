// Package publisher writes a rendered Markdown report somewhere durable.
// Two implementations ship: local disk (atomic write, refuse-first), and
// GitLab (file via Files API on a branch + MR for review). The pipeline
// orchestrator depends only on the Publisher interface, so either sink
// can be substituted without touching run/.
//
// Idempotency is up to each Publisher. LocalPublisher refuses to overwrite
// existing files unless force=true; GitLabPublisher targets the same
// source-branch for a given period so re-runs accumulate commits on one MR.
package publisher

import "context"

// FileKind controls how the publisher treats the report. Future use:
// distinguishes per-project vs rollup, or weekly vs monthly.
type FileKind string

const (
	KindPerProject FileKind = "project"
	KindRollup     FileKind = "rollup"
)

// Request carries everything a Publisher needs to write a single file.
// Ref is set by the Publisher to whatever identifier the sink uses
// (filesystem path, GitLab web URL, etc.).
type Request struct {
	Name    string   // file basename, e.g. "group-proj--7d--customer.md"
	Body    []byte   // raw Markdown bytes (no frontmatter wrapping)
	Force   bool     // overwrite existing target if true
	Kind    FileKind // per-project or rollup
	Period  string   // "2026-10-19" or "2026-W42" — the MR branch slug
	Title   string   // "customer" / "team" — used for frontmatter + MR title
	Project string   // GitLab project path — used for frontmatter UID + tags

	// Public controls docbuilder's external publication flag (the
	// `public: true` frontmatter key). True means docbuilder will index
	// the document for external/customer visibility; false means
	// internal-only. Caller should set this true for customer-audience
	// reports and false for team/internal ones.
	Public bool
}

// Result is what a Publisher returns from Publish.
type Result struct {
	Ref    string // display path / URL for the run summary log
	Branch string // GitLab branch the file landed on; empty for local
	MRURL  string // GitLab MR web URL when one was created or updated; empty for local
}

// Publisher writes one Markdown file at a time. Implementations are
// expected to be safe to call concurrently (the pipeline runs the
// per-project and rollup passes sequentially, but the per-audience
// LLM-then-write pairs could run in parallel in the future).
type Publisher interface {
	Publish(ctx context.Context, req Request) (Result, error)
}
