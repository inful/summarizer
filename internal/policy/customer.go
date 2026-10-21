package policy

import "strings"

// CustomerLabelPrefix marks which customer an epic belongs to. A label
// of the form `org::ACME` attributes the epic to customer ACME.
//
// Convention chosen by the operator: one label, exact prefix, exact
// token — same case-sensitivity rule as TopicSummarize above, so
// `ORG::ACME` is a different label and buckets separately (loudly, as
// its own customer) rather than silently merging into ACME.
const CustomerLabelPrefix = "org::"

// CustomerCode returns the customer code from an epic's labels, and
// whether one was found. Epics with no `org::` label report (false) and
// stay in the combined rollup — nothing is dropped.
//
// When an epic carries more than one `org::` label it spans more than
// one customer; the first wins. GitLab returns labels in a stable
// order, so this is reproducible rather than arbitrary, and the epic
// still appears in that customer's report.
func CustomerCode(labels []string) (string, bool) {
	for _, l := range labels {
		if !strings.HasPrefix(l, CustomerLabelPrefix) {
			continue
		}
		code := strings.TrimSpace(strings.TrimPrefix(l, CustomerLabelPrefix))
		if code == "" {
			// `org::` or `org:: ` carries no customer; keep scanning
			// in case a real one follows.
			continue
		}
		return code, true
	}
	return "", false
}
