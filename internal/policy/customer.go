package policy

import "strings"

// CustomerLabelPrefix marks which customer an epic belongs to. A label
// of the form `org::ACME` attributes the epic to customer ACME.
//
// Convention chosen by the operator: exact prefix, case-sensitive
// (same rule as TopicSummarize above), so `ORG::ACME` is a different
// label and buckets separately rather than silently merging into ACME.
const CustomerLabelPrefix = "org::"

// CustomerCodes returns every customer code found in labels, in the
// order GitLab returns them. An epic can carry more than one
// `org::CODE` label — it spans that many customers, and the
// cross-project rollup must surface it in every one.
//
// A label with a real code is kept (whitespace around the code is
// trimmed); a label of just `org::` or `org:: ` has no code and is
// skipped rather than mis-bucketing the epic under an empty name.
// Duplicate codes collapse to one entry so a misconfigured epic
// doesn't get a duplicate report.
//
// Returns nil (not an empty slice) when no customer codes are
// present, so the caller can distinguish "this epic is unlabelled"
// from "this epic is labelled with an empty code".
func CustomerCodes(labels []string) []string {
	var out []string
	seen := make(map[string]bool, 2)
	for _, l := range labels {
		if !strings.HasPrefix(l, CustomerLabelPrefix) {
			continue
		}
		code := strings.TrimSpace(strings.TrimPrefix(l, CustomerLabelPrefix))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out
}
