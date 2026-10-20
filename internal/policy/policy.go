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
	// TopicSummarize is the base opt-in. On a discovered project,
	// it flips the default from team-only to full output (customer +
	// team). On an explicit-listed project, it's a no-op (those
	// default to full output already).
	TopicSummarize = "summarize"

	// TopicSummarizeTeamOnly is the override. Wherever it appears,
	// the project produces team output only. It's sticky across all
	// sources (config + CLI + topic): if any of them says "team only",
	// team only wins.
	TopicSummarizeTeamOnly = "summarize-team-only"
)

// ProjectSource identifies how the project entered the run. Used by
// the classifier to apply the right default (discovered = safe team
// only; explicit-listed = trust operator's choice).
type ProjectSource int

const (
	// SourceExplicit is a project named on the CLI via --project or
	// in summarizer.yaml's projects list. Operator has explicitly
	// chosen to include this project; default is to trust that choice.
	SourceExplicit ProjectSource = iota

	// SourceDiscovered is a project surfaced by --discover-group. The
	// operator hasn't chosen it by name; default is team-only to keep
	// the safety property (we don't know what's customer-shareable).
	SourceDiscovered
)

// ProjectPolicy is what summarizer does with one project. Run is
// always true today; the Run field exists so future extensions
// (per-project skip, per-project overrides) can land without
// changing the call sites.
type ProjectPolicy struct {
	Run        bool // always true in v1; reserved for future per-project skip
	TeamOnly   bool // true ⇒ never produce a customer-facing report
	CustomerOn bool // true ⇒ customer-facing report is produced (== !TeamOnly AND source != discovered-without-summarizer)
}

// Classify returns the policy for one project. inList is true when
// the project is listed in summarizer.yaml's internal_only_projects or
// passed via --internal-only-project on the CLI — the union is
// computed at the call site (caller passes one boolean capturing
// whether either source flagged the project).
//
// Two OR-gates drive TeamOnly:
//
//	explicitTeamOnly         := inList OR hasTopic(TopicSummarizeTeamOnly)
//	discoveredWithoutOptIn   := source == SourceDiscovered AND
//	                              NOT hasTopic(TopicSummarize)
//	teamOnly                  := explicitTeamOnly OR discoveredWithoutOptIn
//
// CustomerOn is the negation: !teamOnly.
//
// The second gate is the default-safe default: a project discovered
// via --discover-group without the `summarizer` opt-in lands as
// team-only. The operator must either add the `summarizer` topic
// (project-owner signal) or list the project explicitly via --project
// (operator signal) to get a customer-facing version.
//
// Truth table lives in policy_test.go.
func Classify(source ProjectSource, topics []string, inList bool) ProjectPolicy {
	explicitTeamOnly := inList || hasTopic(topics, TopicSummarizeTeamOnly)
	discoveredWithoutOptIn := source == SourceDiscovered && !hasTopic(topics, TopicSummarize)
	teamOnly := explicitTeamOnly || discoveredWithoutOptIn
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
