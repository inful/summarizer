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

// FileKind controls how the publisher treats the report: per-project,
// cross-project rollup, or whole-month rollup.
type FileKind string

const (
	KindPerProject FileKind = "project"
	KindRollup     FileKind = "rollup"
	// KindMonthly is a rollup over a calendar month rather than an ISO
	// week. It exists so the document says so: sharing KindRollup gave
	// a September month-end file the title "Cross-project rollup
	// customer report — all customer-facing projects, 2026-09", identical in
	// shape to the weekly one and told apart only by the period token.
	KindMonthly FileKind = "monthly"
)

// Request carries everything a Publisher needs to write a single file.
// Ref is set by the Publisher to whatever identifier the sink uses
// (filesystem path, GitLab web URL, etc.).
type Request struct {
	Name    string   // file basename, e.g. "group-proj--wk--customer.md"
	Body    []byte   // raw Markdown bytes (no frontmatter wrapping)
	Kind    FileKind // per-project or rollup
	Period  string   // "2026-W42" or "2026-09" — the MR branch slug
	Project string   // GitLab project path — used for frontmatter UID + tags

	// Audience is "customer" or "team", and is what lands in the
	// frontmatter's audience slot and tag. It used to be passed as
	// Title, which worked only for per-project files: the four rollup
	// paths set Title to "rollup customer", "epics rollup for ACME
	// customer" and so on, so those files shipped a title reading
	// "Cross-project rollup rollup customer report" and an audience
	// *tag* that was a sentence. Audience is its own field so a
	// display label can never leak into a machine-read one.
	//
	// The document's human-readable title is NOT a field: the GitLab
	// publisher builds it from Project/Audience/Period/Kind in
	// NewFrontmatter. Request.Title was set by every caller and read
	// by none, so it looked load-bearing and was not.
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
	Ref string // display path / URL for the run summary log

	// MRURL is the GitLab MR web URL when one was found or created;
	// empty for local. Branch used to sit here too and was read by
	// nothing — the run summary reports Ref, which already carries the
	// branch in its blob URL.
	MRURL string

	// Closed is true when the period's MR had already been merged or
	// closed, so Ref points at a branch that will never reach the
	// default branch again. The write succeeded; the content just has
	// nowhere to go. Callers report it rather than treating it as a
	// failure, because re-running a published week is ordinary.
	Closed bool
}

// Publisher writes one Markdown file at a time.
//
// Implementations are safe to call concurrently for DIFFERENT periods.
// They are not safe for concurrent calls on the SAME period: the
// GitLab publisher's MR lookup is a check-then-act — read the memo,
// query three MR states, POST if all empty — and the mutex guards the
// memo without covering that sequence, so two callers for one period
// can each create a merge request. GitLab permits multiple open MRs from
// one source branch, so nothing rejects the second.
//
// The pipeline is sequential, so this never fires today. It is written
// down because the earlier version of this comment said "safe to call
// concurrently" and then named the refactor that breaks it: "the
// per-audience LLM-then-write pairs could run in parallel in the
// future". If you parallelise the per-audience loop, do the MR creation
// under a per-branch sync.Once first. The per-project loop has the same
// exposure — run.Pipeline holds three unsynchronised per-run maps — so
// parallelising that needs a mutex added as well.
//
// TestGitLabPublisher_ConcurrentPublishAcrossBranchesIsRaceFree pins
// the half of this that does hold.
type Publisher interface {
	Publish(ctx context.Context, req Request) (Result, error)
}
