package policy

import (
	"strings"

	"github.com/inful/summarizer/internal/gitlab"
)

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

// EpicCustomerCodes returns the union of every customer code that
// names this epic. The epic's own org::* labels are the strongest
// signal, but the org::* labels on its child issues count too —
// the operator may tag customer-named work at the issue level when
// the parent epic spans more than one customer or when the epic
// itself predates the customer-attribution convention. The result
// is the union, deduped, in stable order (epic labels first, then
// issues in IID order).
//
// Returns nil when neither the epic nor any of its child issues
// carries an org:: label — the caller uses that to decide whether
// the epic belongs in a customer bucket or in the unlabelled
// backlog. nil is distinct from an empty slice so the test pinning
// the contract is honest.
func EpicCustomerCodes(epic *gitlab.Epic) []string {
	if epic == nil {
		return nil
	}
	seen := make(map[string]bool, 2)
	var out []string
	add := func(labels []string) {
		for _, code := range CustomerCodes(labels) {
			if seen[code] {
				continue
			}
			seen[code] = true
			out = append(out, code)
		}
	}
	// Epic's own labels come first so a tag on the parent wins the
	// sort order over a tag on a child.
	add(epic.Labels)
	for _, iss := range epic.Issues {
		add(iss.Labels)
	}
	return out
}
