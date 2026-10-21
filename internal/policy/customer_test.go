package policy

import "testing"

// TestCustomerCode: the `org::CUSTOMER_CODE` label convention. Exact
// `org::` prefix, case-sensitive like every other topic in this
// package, so `ORG::ACME` and `org::acme-beta` don't silently bucket
// into ACME.
func TestCustomerCode(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
		wantOK bool
	}{
		{"nil labels", nil, "", false},
		{"empty labels", []string{}, "", false},
		{"single match", []string{"org::ACME"}, "ACME", true},
		{"match among others", []string{"bug", "org::ACME", "priority::high"}, "ACME", true},
		{"no match", []string{"bug", "ruby"}, "", false},
		{"prefix only, no code", []string{"org::"}, "", false},
		{"prefix only, whitespace code", []string{"org:: "}, "", false},
		{"wrong prefix", []string{"customer::ACME"}, "", false},
		{"uppercase prefix is not a match", []string{"ORG::ACME"}, "", false},
		{"mixed-case prefix is not a match", []string{"Org::ACME"}, "", false},
		{"code case is preserved", []string{"org::Acme Corp"}, "Acme Corp", true},
		{"similar topic does not match", []string{"org::ACME-beta"}, "ACME-beta", true},
		{"double-colon inside code is kept whole", []string{"org::A::B"}, "A::B", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CustomerCode(c.labels)
			if ok != c.wantOK || got != c.want {
				t.Errorf("CustomerCode(%v) = %q,%v; want %q,%v", c.labels, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// TestCustomerCode_MultipleMatchesFirstWins: an epic can legitimately
// span two customers. Bucketing must be deterministic — GitLab
// returns labels in a stable order, so taking the first is
// reproducible rather than arbitrary.
func TestCustomerCode_MultipleMatchesFirstWins(t *testing.T) {
	got, ok := CustomerCode([]string{"org::ACME", "org::GLOBEX"})
	if !ok || got != "ACME" {
		t.Errorf("got %q,%v; want ACME,true (first match wins)", got, ok)
	}
	// Same input, same answer, every time.
	for i := 0; i < 5; i++ {
		if again, _ := CustomerCode([]string{"org::ACME", "org::GLOBEX"}); again != got {
			t.Fatalf("non-deterministic: %q then %q", got, again)
		}
	}
}

// TestCustomerCode_DeterministicOrdering: because the caller sorts the
// resulting bucket keys, an epic tagged with two customers must always
// land in the same bucket across runs.
func TestCustomerCode_DeterministicOrdering(t *testing.T) {
	labels := []string{"priority::high", "org::GLOBEX", "org::ACME"}
	first, _ := CustomerCode(labels)
	for i := 0; i < 3; i++ {
		got, _ := CustomerCode(labels)
		if got != first {
			t.Fatalf("call %d gave %q, first gave %q", i, got, first)
		}
	}
}
