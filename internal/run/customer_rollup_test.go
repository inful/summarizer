package run

import (
	"context"
	"slices"
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

// TestEpicRollup_IssueLabelInheritsToEpic: the inheritance rule.
// An unlabelled epic whose child issues carry org::ACME is an
// ACME epic — it lands in ACME's bucket and NOT in the unlabelled
// backlog. This is the case the per-issue labelling convention
// produces: the operator tags customer-named work at the issue
// level, and the rollup has to follow.
func TestEpicRollup_IssueLabelInheritsToEpic(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	// Epic has NO org:: label. Its two issues do.
	epicShared := epic(1, "shared-work")
	epicShared.Issues = []gitlab.Issue{
		{IID: 10, Labels: []string{"org::ACME"}},
		{IID: 11, Labels: []string{"org::ACME", "priority::high"}},
	}
	epicBacklog := epic(2, "backlog")
	epicBacklog.Issues = []gitlab.Issue{
		{IID: 20, Labels: []string{"ruby"}}, // no org::
	}
	projects := []ProjectResult{
		{Project: "a", CustomerOn: true, ActiveEpics: []*gitlab.Epic{epicShared, epicBacklog}},
	}

	_, err := sp.p.runEpicRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}

	// Files published: ACME, customer combined, team combined.
	// The customer combined holds ONLY the unlabelled epic
	// (the one whose issues have no org:: label). shared-work
	// is in the ACME file.
	names := publishedNames(sp)
	for _, want := range []string{
		"combined--epics-acme--7d--customer.md",
		"combined--epics-rollup--7d--customer.md",
		"combined--epics-rollup--7d--team.md",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("missing file %q in %v", want, names)
		}
	}

	// The first LLM call is for the ACME bucket. The shared-work
	// epic should appear in the ACME prompt, the backlog epic should
	// not.
	acme := sp.llm.promptAt(0)
	if !strings.Contains(acme, "shared-work") {
		t.Errorf("ACME prompt should include the unlabelled epic with org::ACME child issues:\n%s", acme)
	}
	if strings.Contains(acme, "backlog") {
		t.Errorf("ACME prompt must NOT include the truly-unlabelled backlog epic:\n%s", acme)
	}

	// The customer combined is for the backlog file. shared-work
	// must not appear there — it's in ACME's file.
	for _, w := range sp.pub.written {
		if strings.HasSuffix(w.Name, "--customer.md") && strings.Contains(w.Name, "epics-rollup") {
			if strings.Contains(string(w.Body), "shared-work") {
				t.Errorf("ACME-attributed epic leaked into the customer combined file:\n%s", w.Body)
			}
		}
	}
}

// two org:: labels surfaces in both customer files — the whole
// point of one epic spanning two customers. The assertion is
// bucket → file, not bucket → file content (the LLM stub is opaque
// to the bucketing logic).
func TestEpicRollup_MultiCustomerEpicLandsInAllBuckets(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{
		{Project: "a", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
			epic(1, "shared", "org::ACME", "org::GLOBEX"),
		}},
		{Project: "b", CustomerOn: true, ActiveEpics: []*gitlab.Epic{
			epic(2, "acme-only", "org::ACME"),
		}},
	}

	files, err := sp.p.runEpicRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	// Expect: ACME file, GLOBEX file, team combined. No unlabelled
	// epics in this test, so the customer combined file is skipped.
	wantFiles := 3
	if len(files) != wantFiles {
		t.Fatalf("want %d files, got %d: %v", wantFiles, len(files), files)
	}
	// The bucketing decision is in which files exist, not what
	// they say. Check by filename.
	wantNames := map[string]bool{
		"combined--epics-acme--7d--customer.md":   false,
		"combined--epics-globex--7d--customer.md": false,
		"combined--epics-rollup--7d--team.md":     false,
	}
	for _, w := range sp.pub.written {
		name := strings.TrimPrefix(w.Name, "captured/")
		if _, expected := wantNames[name]; expected {
			wantNames[name] = true
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("missing expected file: %s", name)
		}
	}
	// The LLM was called once per file (2 customer + 1 team). Each
	// call has its own prompt; the shared epic's title was in the
	// input prompt for both ACME's and GLOBEX's calls. Inspect
	// those prompts to confirm the bucket shape, not the opaque
	// "rendered" response.
	// The first 2 LLM calls (call indices 0 and 1) are the customer
	// files, in bucket order: ACME, then GLOBEX. The team combined
	// follows. The labels in the LLM stub are not preserved
	// through Publish, so we match on the prompt content: ACME's
	// prompt is the only one that includes acme-only.
	acme := sp.llm.promptAt(0)
	globex := sp.llm.promptAt(1)
	if !strings.Contains(acme, "shared") {
		t.Errorf("ACME prompt should include the shared epic, got:\n%s", acme)
	}
	if !strings.Contains(globex, "shared") {
		t.Errorf("GLOBEX prompt should include the shared epic, got:\n%s", globex)
	}
	if !strings.Contains(acme, "acme-only") {
		t.Errorf("ACME prompt should include the acme-only epic, got:\n%s", acme)
	}
	if strings.Contains(globex, "acme-only") {
		t.Errorf("GLOBEX prompt should NOT include the acme-only epic, got:\n%s", globex)
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
		if !slices.Contains(names, w) {
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
