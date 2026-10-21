package policy

import (
	"reflect"
	"testing"

	"github.com/inful/summarizer/internal/gitlab"
)

// issue builds a minimal Issue carrying the given labels. The shape
// the policy package needs is the IID and the labels.
func issue(iid int, labels ...string) gitlab.Issue {
	return gitlab.Issue{IID: iid, Labels: labels}
}

// TestEpicCustomerCodes_Inheritance: the rule. An epic's customer
// codes are the union of the epic's own org::* labels and the org::
// labels on its child issues. Customers not labelled anywhere on the
// epic don't appear.
func TestEpicCustomerCodes_Inheritance(t *testing.T) {
	cases := []struct {
		name string
		epic *gitlab.Epic
		want []string
	}{
		{"nil epic", nil, nil},
		{"unlabelled epic, no issues", &gitlab.Epic{}, nil},
		{"epic labelled, no issues", &gitlab.Epic{Labels: []string{"org::ACME"}}, []string{"ACME"}},
		{"issues labelled, epic unlabelled", &gitlab.Epic{Issues: []gitlab.Issue{
			issue(1, "org::ACME"),
		}}, []string{"ACME"}},
		{"both labelled, same customer", &gitlab.Epic{
			Labels: []string{"org::ACME"},
			Issues: []gitlab.Issue{issue(1, "org::ACME")},
		}, []string{"ACME"}},
		{"both labelled, different customers", &gitlab.Epic{
			Labels: []string{"org::ACME"},
			Issues: []gitlab.Issue{issue(1, "org::GLOBEX")},
		}, []string{"ACME", "GLOBEX"}},
		{"issues from two customers, epic unlabelled", &gitlab.Epic{Issues: []gitlab.Issue{
			issue(1, "org::ACME"),
			issue(2, "org::GLOBEX"),
		}}, []string{"ACME", "GLOBEX"}},
		{"order is the union, deduped", &gitlab.Epic{
			Labels: []string{"org::GLOBEX"},
			Issues: []gitlab.Issue{issue(1, "org::ACME"), issue(2, "org::ACME")},
		}, []string{"GLOBEX", "ACME"}},
		{"non-org labels do not count", &gitlab.Epic{
			Labels: []string{"priority::high"},
			Issues: []gitlab.Issue{issue(1, "billing", "org::ACME")},
		}, []string{"ACME"}},
		{"empty code is skipped", &gitlab.Epic{
			Labels: []string{"org::"},
			Issues: []gitlab.Issue{issue(1, "org:: ", "org::ACME")},
		}, []string{"ACME"}},
		{"case-sensitive", &gitlab.Epic{
			Labels: []string{"ORG::ACME"},
			Issues: []gitlab.Issue{issue(1, "Org::ACME", "org::ACME")},
		}, []string{"ACME"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EpicCustomerCodes(c.epic)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("EpicCustomerCodes = %v, want %v", got, c.want)
			}
		})
	}
}

// TestEpicCustomerCodes_BacklogContract: the only way an epic
// reaches the unlabelled backlog is if neither the epic nor any of
// its child issues carries an org:: label. The function returns
// codes (non-empty) when the epic is attributable, and nil/empty
// when it is not. The caller uses that to decide which bucket.
func TestEpicCustomerCodes_BacklogContract(t *testing.T) {
	if got := EpicCustomerCodes(&gitlab.Epic{Labels: []string{"org::ACME"}}); len(got) == 0 {
		t.Errorf("epic with its own org::ACME label should return [ACME]; got %v", got)
	}
	if got := EpicCustomerCodes(&gitlab.Epic{Issues: []gitlab.Issue{issue(1, "org::ACME")}}); len(got) == 0 {
		t.Errorf("epic with a child issue labelled org::ACME should return [ACME]; got %v", got)
	}
	if got := EpicCustomerCodes(&gitlab.Epic{Labels: []string{"ruby", "billing"}}); len(got) != 0 {
		t.Errorf("epic with no org:: anywhere should return nil/empty; got %v", got)
	}
}
