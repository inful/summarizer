package run

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/prompt"
)

// epic builds an active epic carrying the given labels.
func epic(iid int, title string, labels ...string) *gitlab.Epic {
	return &gitlab.Epic{IID: iid, Title: title, State: "opened", WebURL: "u/" + title, Labels: labels}
}

// names extracts the filename from each captured publish. We just
// match on the whole basename rather than re-parse it.
func publishedNames(sp *sourcePipeline) []string {
	var out []string
	for _, w := range sp.pub.written {
		out = append(out, w.Name)
	}
	return out
}

// TestEpicRollup_SplitsCustomerAudienceByOrgLabel is the headline
// behaviour: the customer-facing epic rollup becomes one file per
// `org::CUSTOMER_CODE`, plus one combined file for the epics nobody
// has labelled yet.
func TestEpicRollup_SplitsCustomerAudienceByOrgLabel(t *testing.T) {
	projects := []ProjectResult{
		{Project: "a", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
			epic(1, "alpha", "org::ACME"),
			epic(2, "beta"),
		}},
		{Project: "b", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
			epic(3, "gamma", "org::ACME", "priority::high"),
			epic(4, "delta", "org::GLOBEX"),
		}},
	}

	got := bucketEpicsByCustomer(projects, 0)

	var codes []string
	for _, b := range got {
		codes = append(codes, b.Customer)
	}
	sort.Strings(codes)
	want := []string{"", "ACME", "GLOBEX"}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("buckets = %v, want %v", codes, want)
	}

	// ACME's bucket holds the two epics labelled for it, from two
	// different projects — cross-project is the whole point.
	for _, b := range got {
		switch b.Customer {
		case "ACME":
			if len(b.Entries) != 2 {
				t.Fatalf("ACME: want 2 project-entries, got %d: %+v", len(b.Entries), b.Entries)
			}
			titles := []string{b.Entries[0].Epics[0].Epic.Title, b.Entries[1].Epics[0].Epic.Title}
			if titles[0] != "alpha" || titles[1] != "gamma" {
				t.Errorf("ACME epics = %v, want [alpha gamma]", titles)
			}
		case "GLOBEX":
			if len(b.Entries) != 1 || b.Entries[0].Epics[0].Epic.Title != "delta" {
				t.Errorf("GLOBEX bucket wrong: %+v", b.Entries)
			}
		case "":
			if len(b.Entries) != 1 || b.Entries[0].Epics[0].Epic.Title != "beta" {
				t.Errorf("unlabelled bucket should hold only 'beta': %+v", b.Entries)
			}
		}
	}
}

// TestEpicRollup_UnlabelledIsLast: the combined bucket sorts after the
// customer codes so the branch file order is stable and the backlog
// file lands in a predictable place.
func TestEpicRollup_UnlabelledIsLast(t *testing.T) {
	projects := []ProjectResult{{Project: "a", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
		epic(1, "plain"),
		epic(2, "z", "org::ZULU"),
		epic(3, "a", "org::ALPHA"),
	}}}
	got := bucketEpicsByCustomer(projects, 0)
	if len(got) != 3 {
		t.Fatalf("want 3 buckets, got %d", len(got))
	}
	if got[0].Customer != "ALPHA" || got[1].Customer != "ZULU" || got[2].Customer != "" {
		t.Errorf("order = %q,%q,%q; want ALPHA,ZULU,<empty>",
			got[0].Customer, got[1].Customer, got[2].Customer)
	}
}

// TestEpicRollup_TeamAudienceIsNotSplit: internal readers get the whole
// picture in one file, unlabelled epics included.
func TestEpicRollup_TeamAudienceIsNotSplit(t *testing.T) {
	projects := []ProjectResult{
		{Project: "a", ActiveEpics: []*gitlab.Epic{epic(1, "alpha", "org::ACME")}},
		{Project: "b", ActiveEpics: []*gitlab.Epic{epic(2, "plain")}},
	}
	got := bucketAllEpics(projects)
	if len(got) != 1 {
		t.Fatalf("team audience should produce 1 part, got %d", len(got))
	}
	if len(got[0].Entries) != 2 {
		t.Errorf("want both projects in the one combined part, got %d", len(got[0].Entries))
	}
	if bucketAllEpics(nil) != nil {
		t.Error("no epics should yield no parts")
	}
}

// TestEpicRollup_CustomerOnGateAppliesPerBucket: the D5 audience
// boundary must still hold once the rollup is split. A team-only
// project's epic must not appear in ANY customer file — not just be
// absent from the combined one.
func TestEpicRollup_CustomerOnGateAppliesPerBucket(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{
		{Project: "public", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
			epic(1, "pub-acme", "org::ACME"), epic(2, "pub-plain"),
		}},
		{Project: "secret", TeamOnly: true, CustomerOn: false, ActiveEpics: []*gitlab.Epic{
			epic(3, "SECRET-ACME", "org::ACME"), epic(4, "secret-plain"),
		}},
	}

	files, err := sp.p.runEpicRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	if len(files) != 3 { // acme + combined, customer; combined, team
		t.Fatalf("want 3 files, got %d: %v", len(files), files)
	}

	for _, w := range sp.pub.written {
		body := string(w.Body)
		if strings.Contains(body, "SECRET-ACME") {
			t.Errorf("team-only epic leaked into %s", w.Name)
		}
		if strings.Contains(body, "secret-plain") {
			t.Errorf("team-only epic leaked into %s", w.Name)
		}
		if !w.Public && strings.Contains(body, "pub-acme") {
			t.Errorf("customer-eligible epic missing from the team file %s", w.Name)
		}
	}

	names := publishedNames(sp)
	wantNames := []string{"combined--epics-acme--7d--customer.md", "combined--epics-rollup--7d--customer.md", "combined--epics-rollup--7d--team.md"}
	for _, w := range wantNames {
		if !containsString(names, w) {
			t.Errorf("missing %s; got %v", w, names)
		}
	}
}

// TestEpicRollup_PublicFlagPerAudience: only customer files are
// Public. A per-customer file must not inherit the team audience's
// internal-only default or vice versa.
func TestEpicRollup_PublicFlagPerAudience(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{{Project: "a", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
		epic(1, "x", "org::ACME"), epic(2, "y"),
	}}}
	if _, err := sp.p.runEpicRollup(context.Background(), projects); err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	for _, w := range sp.pub.written {
		isCustomerFile := strings.HasSuffix(w.Name, "--customer.md")
		if w.Public != isCustomerFile {
			t.Errorf("%s: Public=%v, want %v", w.Name, w.Public, isCustomerFile)
		}
	}
}

// TestEpicRollup_NoEpicsSkipsEverything: no active epics → no files,
// not empty ones.
func TestEpicRollup_NoEpicsSkipsEverything(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	files, err := sp.p.runEpicRollup(context.Background(), []ProjectResult{{Project: "a", CustomerOn: true}})
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("want no files, got %v", files)
	}
	if len(sp.pub.written) != 0 {
		t.Errorf("published empty rollups: %v", publishedNames(sp))
	}
}

// TestSlugCustomer: filename safety without turning the code into
// something unreadable.
func TestSlugCustomer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ACME", "acme"},
		{"Acme Corp", "acme-corp"},
		{"  ACME--EU  ", "acme-eu"},
		{"Acme  Corp", "acme-corp"},
		{"ACME_2024", "acme-2024"},
		{"Acme/Corp", "acme-corp"},
		{"ACME::EU", "acme-eu"},
		{"...", ""},
	}
	for _, c := range cases {
		if got := slugCustomer(c.in); got != c.want {
			t.Errorf("slugCustomer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEpicRollupFilename_ForCustomerCode: the exact file names an
// operator will see in the period directory.
func TestEpicRollupFilename_ForCustomerCode(t *testing.T) {
	got := rollupFilename(7*24*time.Hour, prompt.AudienceCustomer, "epics-acme")
	want := "combined--epics-acme--7d--customer.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
