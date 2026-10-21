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
	Kind    FileKind // per-project or rollup
	Period  string   // "2026-10-19" or "2026-W42" — the MR branch slug
	Title   string   // human label for the MR, e.g. "epics rollup for ACME customer"
	Project string   // GitLab project path — used for frontmatter UID + tags

	// Audience is "customer" or "team", and is what lands in the
	// frontmatter's audience slot and tag. It used to be passed as
	// Title, which worked only for per-project files: the four rollup
	// paths set Title to "rollup customer", "epics rollup for ACME
	// customer" and so on, so those files shipped a title reading
	// "Cross-project rollup rollup customer report" and an audience
	// *tag* that was a sentence. Audience is its own field so a
	// display label can never leak into a machine-read one.
	Audience string

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

	// Closed is true when the period's MR had already been merged or
	// closed, so Ref points at a branch that will never reach the
	// default branch again. The write succeeded; the content just has
	// nowhere to go. Callers report it rather than treating it as a
	// failure, because re-running a published week is ordinary.
	Closed bool
}

// Publisher writes one Markdown file at a time. Implementations are
// expected to be safe to call concurrently (the pipeline runs the
// per-project and rollup passes sequentially, but the per-audience
// LLM-then-write pairs could run in parallel in the future).
type Publisher interface {
	Publish(ctx context.Context, req Request) (Result, error)
}
