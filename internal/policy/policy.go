// Package policy decides what summarizer does with one project, given
// where the project came from and its GitLab metadata. The decision
// affects three downstream surfaces:
//   - runOneProject's audience loop (skip the customer iteration
//     when CustomerOn is false)
//   - runRollup's customer-audience filter
//   - runEpicRollup's customer-audience filter
//
// The classifier is a pure function (no I/O, no goroutines) so it's
// cheap to call from many call sites and easy to fuzz or table-test.
// See policy_test.go for the truth table.
package policy

// Topic markers summarizer recognises on GitLab projects. Hyphen
// format per the operator's convention; GitLab topic tokens allow
// alphanumerics, hyphens, dots, underscores. Topics are case-
// sensitive — see TestClassify_TopicCaseSensitive.
const (
	// TopicSummarize opts the project in to customer-facing output.
	// It is the only way to produce a non-team-only report.
	TopicSummarize = "summarize"

	// TopicSummarizeTeamOnly is the team-only override. Wherever it
	// appears, the project produces team output only. Sticky across
	// all signals: if any of them says "team only", team only wins.
	TopicSummarizeTeamOnly = "summarize-team-only"
)

// ProjectSource identifies how the project entered the run. The
// classifier itself no longer reads this value (the audience
// decision is uniform regardless of source), but the type is kept
// in the signature for caller readability and for any future
// source-specific branching.
type ProjectSource int

const (
	// SourceExplicit is a project named on the CLI via --project or
	// in summarizer.yaml's projects list.
	SourceExplicit ProjectSource = iota

	// SourceDiscovered is a project surfaced by --discover-group,
	// --discover-by-topic, or the default-by-activity discovery.
	SourceDiscovered
)

// ProjectPolicy is what summarizer does with one project. Run is
// always true today; the Run field exists so future extensions
// (per-project skip, per-project overrides) can land without
// changing the call sites.
type ProjectPolicy struct {
	Run        bool // always true in v1; reserved for future per-project skip
	TeamOnly   bool // true ⇒ never produce a customer-facing report
	CustomerOn bool // true ⇒ customer-facing report is produced
}

// Classify returns the policy for one project. inList is true when
// the project is listed in summarizer.yaml's internal_only_projects
// or passed via --internal-only-project on the CLI — the union is
// computed at the call site (caller passes one boolean capturing
// whether either source flagged the project).
//
// One OR-gate drives TeamOnly:
//
//	teamOnly := inList
//	            OR hasTopic(TopicSummarizeTeamOnly)   // team-only override topic
//	            OR NOT hasTopic(TopicSummarize)        // default: no customer opt-in
//
// CustomerOn is the negation: !teamOnly. The default — no opt-in
// topic and no operator override — is team-only. The
// `summarize` topic is the only way to produce a customer-facing
// report.
//
// The previous model distinguished discovered from explicit at this
// level (discovered defaulting to team-only, explicit defaulting to
// customer-on). That distinguished was removed when the operator
// inverted the discovery model from "opt in to summarizer" to
// "opt out of summarizer": every active project now runs by
// default, and the audience default is uniformly team-only. The
// operator's opt-in to customer is a single bit — the
// `summarize` topic — for every project regardless of source.
//
// The project-level opt-out (file `.summaryignore` or `.docignore`
// at the repo root) is NOT modelled here. It is checked at the
// pipeline layer because it requires a Files API call; if the file
// is present the project is excluded from the run entirely before
// this function is even called.
//
// Truth table lives in policy_test.go.
func Classify(source ProjectSource, topics []string, inList bool) ProjectPolicy {
	teamOnly := inList ||
		hasTopic(topics, TopicSummarizeTeamOnly) ||
		!hasTopic(topics, TopicSummarize)
	return ProjectPolicy{
		Run:        true,
		TeamOnly:   teamOnly,
		CustomerOn: !teamOnly,
	}
}

// hasTopic is an exact-token lookup. Empty input never matches.
// Topic labels are tokens — substring or prefix matching would be
// wrong (a topic "summarize-beta" should NOT match "summarize").
func hasTopic(topics []string, want string) bool {
	for _, t := range topics {
		if t == want {
			return true
		}
	}
	return false
}
