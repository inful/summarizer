package policy

import (
	"reflect"
	"testing"
)

// TestCustomerCodes: the `org::CUSTOMER_CODE` label convention. An
// epic can carry more than one org:: label (it spans more than one
// customer), and every code returns. Code case is preserved; prefix
// is case-sensitive.
func TestCustomerCodes(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   []string
	}{
		{"nil labels", nil, nil},
		{"empty labels", []string{}, nil},
		{"single match", []string{"org::ACME"}, []string{"ACME"}},
		{"match among others", []string{"bug", "org::ACME", "priority::high"}, []string{"ACME"}},
		{"no match", []string{"bug", "ruby"}, nil},
		{"prefix only, no code", []string{"org::"}, nil},
		{"prefix only, whitespace code", []string{"org:: "}, nil},
		{"wrong prefix", []string{"customer::ACME"}, nil},
		{"uppercase prefix is not a match", []string{"ORG::ACME"}, nil},
		{"mixed-case prefix is not a match", []string{"Org::ACME"}, nil},
		{"code case is preserved", []string{"org::Acme Corp"}, []string{"Acme Corp"}},
		{"similar topic does not match", []string{"org::ACME-beta"}, []string{"ACME-beta"}},
		{"double-colon inside code is kept whole", []string{"org::A::B"}, []string{"A::B"}},
		{"two customers, all returned", []string{"org::ACME", "org::GLOBEX"}, []string{"ACME", "GLOBEX"}},
		{"two customers, order is label order", []string{"org::GLOBEX", "org::ACME"}, []string{"GLOBEX", "ACME"}},
		{"duplicate code is one entry", []string{"org::ACME", "org::ACME"}, []string{"ACME"}},
		{"empty code among real ones is skipped", []string{"org::", "org::ACME", "org:: "}, []string{"ACME"}},
		{"unrelated labels do not block", []string{"ruby", "billing", "org::ACME", "priority::high"}, []string{"ACME"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CustomerCodes(c.labels)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("CustomerCodes(%v) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// TestCustomerCodes_Deterministic: the same input yields the same
// output, every time, so two runs of the same window produce the
// same set of bucket memberships.
func TestCustomerCodes_Deterministic(t *testing.T) {
	labels := []string{"priority::high", "org::GLOBEX", "org::ACME"}
	first := CustomerCodes(labels)
	for i := 0; i < 5; i++ {
		got := CustomerCodes(labels)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("non-deterministic: %v then %v", first, got)
		}
	}
}
