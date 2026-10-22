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

import "slices"

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

	// TopicNoSummarize opts the project out of the run entirely, as an
	// alternative to the .summaryignore / .docignore file.
	//
	// Why both: the file is the better signal — version-controlled,
	// greppable, and visible to everyone who clones the repo — but it
	// needs a commit and a merge, and it puts noise in a project's
	// history for something that is not a code change. A topic is a
	// one-click reversible toggle, which is what you want for "noisy
	// this sprint, skip it for a fortnight", or a repo you administer in
	// GitLab but should not be pushing branches into.
	//
	// The cost is visibility: a topic is invisible to anyone reading the
	// repository, so the reason is named in the discovery log line.
	//
	// Naming follows GitLab's own exclusion convention (`no-issue`,
	// `no label`, `skip ci`) rather than this repo's `summarize-*`
	// family, so it reads as an exclusion to anyone who already knows
	// GitLab and does not read as a third summarizer mode.
	TopicNoSummarize = "no-summarize"
)

// ProjectPolicy is what summarizer does with one project.
type ProjectPolicy struct {
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
//	            OR slices.Contains(topics, TopicSummarizeTeamOnly)
//	            OR NOT slices.Contains(topics, TopicSummarize)
//
// CustomerOn is the negation: !teamOnly. The default — no opt-in
// topic and no operator override — is team-only. The
// `summarize` topic is the only way to produce a customer-facing
// report.
//
// How the project entered the run is deliberately NOT an input. The
// previous model distinguished discovered from explicit here
// (discovered defaulting to team-only, explicit defaulting to
// customer-on); that distinction was removed when the operator
// inverted the discovery model from "opt in to summarizer" to
// "opt out of summarizer". Every active project runs by default and
// the audience default is uniformly team-only, so the source cannot
// change the answer.
//
// The project-level opt-out (file `.summaryignore` or `.docignore`
// at the repo root) is NOT modelled here. It is checked at the
// pipeline layer because it requires a Files API call; if the file
// is present the project is excluded from the run entirely before
// this function is even called.
//
// Truth table lives in policy_test.go.
func Classify(topics []string, inList bool) ProjectPolicy {
	teamOnly := inList ||
		slices.Contains(topics, TopicSummarizeTeamOnly) ||
		!slices.Contains(topics, TopicSummarize)
	return ProjectPolicy{
		TeamOnly:   teamOnly,
		CustomerOn: !teamOnly,
	}
}
