package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/events"
	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/policy"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/publisher"
)

// epic builds an active epic carrying the given labels.
func epic(iid int, title string, labels ...string) *gitlab.Epic {
	return &gitlab.Epic{IID: iid, Title: title, State: "opened", WebURL: "u/" + title, Labels: labels}
}

// active builds a customer-on ProjectResult whose epics all counted
// one event in this project, split by each epic's own org:: codes.
//
// The count has to be there. ActiveEpics is the GROUP's list — epics
// are group-scoped — so in production a project carries epics that own
// none of their issues, and the rollups skip those on a zero count.
// A fixture with ActiveEpics and no EpicEventCounts is a state the
// pipeline cannot produce, and asserting on it tests nothing real.
func active(pr ProjectResult, epics ...*gitlab.Epic) ProjectResult {
	pr.ActiveEpics = epics
	pr.EpicEventCounts = map[int]EpicEvents{}
	for _, e := range epics {
		ev := EpicEvents{Total: 1, ByCustomer: map[string]int{}}
		for _, code := range policy.EpicCustomerCodes(e) {
			ev.ByCustomer[code]++
		}
		pr.EpicEventCounts[e.IID] = ev
	}
	return pr
}

// on is active for a customer-on project.
func on(project string, epics ...*gitlab.Epic) ProjectResult {
	return active(ProjectResult{Project: project, CustomerOn: true}, epics...)
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
		on("a", epic(1, "alpha", "org::ACME"), epic(2, "beta")),
		on("b", epic(3, "gamma", "org::ACME", "priority::high"), epic(4, "delta", "org::GLOBEX")),
	}

	got := bucketEpicsByCustomer(projects)

	var codes []string
	for _, b := range got {
		codes = append(codes, b.Customer)
	}
	sort.Strings(codes)
	// No "" bucket. This function only feeds the CUSTOMER audience,
	// which is published with public: true. An epic nobody attributed
	// to a customer is the least attributed thing in the system, and
	// every other audience decision here is opt-in. It still reaches the
	// team rollup through bucketAllEpics.
	want := []string{"ACME", "GLOBEX"}
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
			t.Errorf("an unattributed epic must not reach a customer bucket: %+v", b.Entries)
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
		on("a", epicShared, epicBacklog),
	}

	_, _, _, err := sp.p.runEpicRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}

	// Files published: ACME (customer) and the combined team rollup.
	// There is no customer-side combined file any more: that bucket
	// existed only to carry the unlabelled epic, and an unattributed
	// epic is not customer-visible. shared-work is in the ACME file;
	// backlog is in the team file only.
	names := publishedNames(sp)
	for _, want := range []string{
		epicCustomerFile("ACME"),
		"combined--epics-rollup--wk--team.md",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("missing file %q in %v", want, names)
		}
	}
	if slices.Contains(names, "combined--epics-rollup--wk--customer.md") {
		t.Errorf("no unattributed epic, so no customer combined file: %v", names)
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
		on("a", epic(1, "shared", "org::ACME", "org::GLOBEX")),
		on("b", epic(2, "acme-only", "org::ACME")),
	}

	files, _, _, err := sp.p.runEpicRollup(context.Background(), projects)
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
		epicCustomerFile("ACME"):              false,
		epicCustomerFile("GLOBEX"):            false,
		"combined--epics-rollup--wk--team.md": false,
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

// TestEpicRollup_UnattributedIsNotACustomerBucket: there is no longer
// an unlabelled customer bucket. The combined customer file existed
// only to carry epics nobody attributed, and an unattributed epic is
// not customer-visible — every other audience decision here is opt-in.
// The team audience still sees the whole fleet, via bucketAllEpics.
//
// Customer codes sort lexically so the branch file order is stable.
func TestEpicRollup_UnattributedIsNotACustomerBucket(t *testing.T) {
	projects := []ProjectResult{
		on("a", epic(1, "plain"), epic(2, "z", "org::ZULU"), epic(3, "a", "org::ALPHA")),
	}
	got := bucketEpicsByCustomer(projects)
	if len(got) != 2 {
		t.Fatalf("want 2 buckets (the unlabelled epic is not one of them), got %d", len(got))
	}
	if got[0].Customer != "ALPHA" || got[1].Customer != "ZULU" {
		t.Errorf("order = %q,%q; want ALPHA,ZULU",
			got[0].Customer, got[1].Customer)
	}
	// And the unattributed epic is still reachable for the team.
	if team := bucketAllEpics(projects); len(team) != 1 || len(team[0].Entries) != 1 ||
		len(team[0].Entries[0].Epics) != 3 {
		t.Errorf("the team audience should still see all three epics, got %+v", team)
	}
}

// TestEpicRollup_TeamAudienceIsNotSplit: internal readers get the whole
// picture in one file, unlabelled epics included.
func TestEpicRollup_TeamAudienceIsNotSplit(t *testing.T) {
	projects := []ProjectResult{
		on("a", epic(1, "alpha", "org::ACME")),
		on("b", epic(2, "plain")),
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
		on("public", epic(1, "pub-acme", "org::ACME"), epic(2, "pub-plain")),
		active(ProjectResult{Project: "secret", TeamOnly: true, CustomerOn: false},
			epic(3, "SECRET-ACME", "org::ACME"), epic(4, "secret-plain")),
	}

	files, _, _, err := sp.p.runEpicRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	if len(files) != 2 { // acme, customer; combined, team
		t.Fatalf("want 2 files, got %d: %v", len(files), files)
	}
	// "pub-plain" is unattributed, so it is team-only now — it must not
	// be in the ACME customer file.
	for _, w := range sp.pub.written {
		if strings.Contains(w.Name, "customer") && strings.Contains(string(w.Body), "pub-plain") {
			t.Errorf("unattributed epic reached a customer file: %s", w.Name)
		}
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
	// No customer-side combined file: it only ever carried unattributed
	// epics, which are not customer-visible.
	wantNames := []string{epicCustomerFile("ACME"), "combined--epics-rollup--wk--team.md"}
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
	projects := []ProjectResult{on("a", epic(1, "x", "org::ACME"), epic(2, "y"))}
	if _, _, _, err := sp.p.runEpicRollup(context.Background(), projects); err != nil {
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
	files, _, _, err := sp.p.runEpicRollup(context.Background(), []ProjectResult{{Project: "a", CustomerOn: true}})
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

// epicCustomerFile is the filename a customer bucket publishes under.
// Derived from slugCustomer rather than hardcoded: the slug carries a
// hash of the org:: code, and a literal here would drift from it the
// moment the slug changes.
func epicCustomerFile(code string) string {
	return rollupFilename(prompt.AudienceCustomer, "epics-"+slugCustomer(code))
}

// TestSlugCustomer: filename safety without turning the code into
// something unreadable. Every slug ends in a short hash of the
// ORIGINAL code, because the readable part alone is lossy and a lossy
// customer name is a uid swap between two public documents.
func TestSlugCustomer(t *testing.T) {
	suffix := func(code string) string {
		sum := sha256.Sum256([]byte(code))
		return hex.EncodeToString(sum[:3])
	}
	cases := []struct{ in, want string }{
		{"ACME", "acme-" + suffix("ACME")},
		{"Acme Corp", "acme-corp-" + suffix("Acme Corp")},
		{"  ACME--EU  ", "acme-eu-" + suffix("  ACME--EU  ")},
		{"Acme  Corp", "acme-corp-" + suffix("Acme  Corp")},
		{"ACME_2024", "acme-2024-" + suffix("ACME_2024")},
		{"Acme/Corp", "acme-corp-" + suffix("Acme/Corp")},
		{"ACME::EU", "acme-eu-" + suffix("ACME::EU")},
		{"...", "c-" + suffix("...")},
	}
	for _, c := range cases {
		if got := slugCustomer(c.in); got != c.want {
			t.Errorf("slugCustomer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSlugCustomer_IsInjective: the property the customer filename
// rests on. Two org:: codes that slug to one name write one
// combined--epics-*.md — the second overwrites the first and inherits
// its docbuilder UID, on a file published public: true.
//
// Every pair below is legal label text and collided before the hash
// was added. It is the same failure sanitize() had for project paths.
func TestSlugCustomer_IsInjective(t *testing.T) {
	codes := []string{
		"Acme  EU", "acme.eu", "acme-eu", "ACME-EU", "Acme EU",
		"Acme Corp", "acme-corp", "Acme  Corp", "acme_corp",
		"ACME", "acme", "Acme", "acme.",
		"...", "   ",
	}
	seen := make(map[string]string, len(codes))
	for _, c := range codes {
		got := slugCustomer(c)
		if prev, dup := seen[got]; dup {
			t.Errorf("COLLISION: org::%q and org::%q both slug to %q — one "+
				"public customer report overwrites the other and inherits its uid",
				prev, c, got)
			continue
		}
		seen[got] = c
	}
}

// TestEpicRollupFilename_ForCustomerCode: the exact file names an
// operator will see in the period directory.
func TestEpicRollupFilename_ForCustomerCode(t *testing.T) {
	got := rollupFilename(prompt.AudienceCustomer, "epics-"+slugCustomer("ACME"))
	want := "combined--epics-" + slugCustomer("ACME") + "--wk--customer.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !strings.Contains(got, slugCustomer("ACME")) {
		t.Errorf("the customer slug must survive into the filename: %q", got)
	}
}

// TestEpicRollup_UnattributedEpicNeverReachesACustomerFile: the whole
// point of finding 1. bucketEpicsByCustomer feeds the CUSTOMER
// audience, which is published with public: true — so an epic that
// nobody attributed to a customer was reaching a customer document,
// while every other audience decision in summarizer is opt-in. Here the
// only epic has no org:: label, so no customer file may exist at all.
func TestEpicRollup_UnattributedEpicNeverReachesACustomerFile(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{
		on("public", epic(1, "nobody-claimed-this")),
	}
	if _, _, _, err := sp.p.runEpicRollup(context.Background(), projects); err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	// The stub LLM returns a canned body, so the epic is asserted on
	// the PROMPT — which is what decides what the model is shown.
	sp.llm.mu.Lock()
	calls := append([]recordedCall(nil), sp.llm.calls...)
	sp.llm.mu.Unlock()

	sawTeam := false
	for i, c := range calls {
		hasEpic := strings.Contains(c.userPrompt, "nobody-claimed-this")
		if hasEpic && sawTeam {
			t.Errorf("prompt %d carries the unattributed epic a second time", i)
		}
		sawTeam = sawTeam || hasEpic
	}
	if !sawTeam {
		t.Error("the team rollup should still carry the unattributed epic")
	}
	// And no customer prompt mentions it at all — with only one
	// unattributed epic there is no customer bucket to render.
	for _, w := range sp.pub.written {
		if strings.Contains(w.Name, "customer") {
			t.Errorf("a customer epic file was published with no attributed epic: %s", w.Name)
		}
	}
}

// TestRun_OrdersRollupInputLeastActiveFirst: both rollup budgets drop
// from the FRONT of res.Projects, so the order decides whose work is
// omitted when the prompt exceeds its budget. The order arriving from
// discovery was explicit-project first, then last_activity_at
// DESCENDING — newest activity first — so "drop the first" silently
// dropped the busiest project while both budgets documented it as
// dropping the oldest.
func TestRun_OrdersRollupInputLeastActiveFirst(t *testing.T) {
	p, _ := twoProjectPipeline(t, "")
	p.Opts.Projects = []string{"group/aaa", "group/zzz"}
	p.Opts.Period = "2026-W43"

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// twoProjectPipeline serves both projects the same event count, so
	// the tiebreak is the path — the cohort must be reproducible.
	var got []string
	for _, pr := range res.Projects {
		got = append(got, pr.Project)
	}
	if len(got) != 2 || got[0] != "group/aaa" || got[1] != "group/zzz" {
		t.Errorf("rollup input order = %v; want least-active-first, which for equal counts is path order", got)
	}
}

// TestEpicRollup_SharedEpicCountsArePerCustomer: an epic reaches more
// than one customer bucket on purpose — policy.EpicCustomerCodes
// unions the epic's labels with its issues' — and every one of those
// files is published public: true.
//
// The EventCount used to be the epic's TOTAL in each of them, so a
// shared epic told ACME how many events GLOBEX's issues contributed,
// and told GLOBEX the same about ACME's. Each customer file may only
// report the volume its own customer's issues produced.
func TestEpicRollup_SharedEpicCountsArePerCustomer(t *testing.T) {
	shared := epic(1, "Shared platform work", "org::ACME")
	shared.Issues = []gitlab.Issue{
		{IID: 10, Labels: []string{"org::ACME"}},
		{IID: 11, Labels: []string{"org::GLOBEX"}},
	}
	// Three events under the epic: two ACME issues and one GLOBEX one.
	// The epic's total is 3; each customer file may report only its own.
	evs := []events.NormalizedEvent{
		{Kind: events.KindIssue, Ref: "#10", Labels: []string{"org::ACME"}, Epic: shared},
		{Kind: events.KindIssue, Ref: "#11", Labels: []string{"org::GLOBEX"}, Epic: shared},
		{Kind: events.KindIssue, Ref: "#12", Labels: []string{"org::ACME"}, Epic: shared},
	}
	projects := []ProjectResult{{
		Project:         "g/p",
		CustomerOn:      true,
		ActiveEpics:     []*gitlab.Epic{shared},
		EpicEventCounts: epicEventCounts(evs, []*gitlab.Epic{shared}),
	}}

	want := map[string]int{"ACME": 2, "GLOBEX": 1}
	got := map[string]int{}
	for _, b := range bucketEpicsByCustomer(projects) {
		if len(b.Entries) != 1 || len(b.Entries[0].Epics) != 1 {
			t.Fatalf("%s bucket wrong: %+v", b.Customer, b.Entries)
		}
		got[b.Customer] = b.Entries[0].Epics[0].EventCount
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for code, n := range want {
		if got[code] != n {
			t.Errorf("%s sees %d events, want %d — a shared epic's total in a "+
				"public customer file discloses another customer's volume",
				code, got[code], n)
		}
	}
	// Pin the old value too: the epic's total is 3, and 3 is exactly
	// what used to be printed in both files.
	if total := projects[0].EpicEventCounts[1].Total; total != 3 {
		t.Errorf("epic total = %d, want 3", total)
	}
}

// TestEpicRollup_TeamAudienceKeepsTheTotal: the split is a customer-
// audience concern only. The team file is not published public and
// deliberately shows the whole fleet, so it keeps the epic's full count.
func TestEpicRollup_TeamAudienceKeepsTheTotal(t *testing.T) {
	shared := epic(1, "Shared platform work", "org::ACME")
	shared.Issues = []gitlab.Issue{
		{IID: 10, Labels: []string{"org::ACME"}},
		{IID: 11, Labels: []string{"org::GLOBEX"}},
	}
	evs := []events.NormalizedEvent{
		{Kind: events.KindIssue, Ref: "#10", Labels: []string{"org::ACME"}, Epic: shared},
		{Kind: events.KindIssue, Ref: "#11", Labels: []string{"org::GLOBEX"}, Epic: shared},
	}
	projects := []ProjectResult{{
		Project:         "g/p",
		ActiveEpics:     []*gitlab.Epic{shared},
		EpicEventCounts: epicEventCounts(evs, []*gitlab.Epic{shared}),
	}}

	buckets := bucketAllEpics(projects)
	if len(buckets) != 1 || len(buckets[0].Entries) != 1 {
		t.Fatalf("team bucket wrong: %+v", buckets)
	}
	if n := buckets[0].Entries[0].Epics[0].EventCount; n != 2 {
		t.Errorf("team audience sees %d events, want the epic's total 2", n)
	}
}

// TestEpicRollup_ConfidentialEpicNeverReachesACustomerFile: GitLab's
// confidential flag is the epic owner saying "not for everyone". The
// customer epic rollups are published public: true — docbuilder
// exposes them externally — so a confidential epic must stay out of
// them whatever its org:: labels say. The team file is internal and
// keeps it.
func TestEpicRollup_ConfidentialEpicNeverReachesACustomerFile(t *testing.T) {
	underNDA := epic(2, "under NDA", "org::ACME")
	underNDA.Confidential = true
	projects := []ProjectResult{
		on("g/p", epic(1, "public work", "org::ACME"), underNDA),
	}

	for _, b := range bucketEpicsByCustomer(projects) {
		for _, ent := range b.Entries {
			for _, s := range ent.Epics {
				if s.Epic.Confidential {
					t.Errorf("confidential epic %q reached %s's public file", s.Epic.Title, b.Customer)
				}
			}
		}
	}
	// ACME's file still exists — the public epic keeps it from being
	// empty, so this is a filter, not a suppression.
	got := bucketEpicsByCustomer(projects)
	if len(got) != 1 || len(got[0].Entries[0].Epics) != 1 || got[0].Entries[0].Epics[0].Epic.Title != "public work" {
		t.Errorf("ACME bucket = %+v, want only the public epic", got)
	}
	// The team audience is internal and keeps the whole fleet.
	team := bucketAllEpics(projects)
	if len(team) != 1 || len(team[0].Entries[0].Epics) != 2 {
		t.Errorf("team bucket = %+v, want both epics", team)
	}
}

// TestEpicRollup_PerCustomerFrontmatterNamesTheCustomer: two customer
// epic rollups in one period are two documents in the docbuilder index,
// both published public: true. NewFrontmatter builds its title from
// project/audience/period/kind, and the rollups passed Project: "" —
// so ACME's and GLOBEX's shipped the identical title
// ("Cross-project rollup customer report — all subscribed projects,
// 2026-W43") and identical tags. Nothing in the index said which
// customer a document belonged to.
func TestEpicRollup_PerCustomerFrontmatterNamesTheCustomer(t *testing.T) {
	projects := []ProjectResult{
		on("a", epic(1, "alpha", "org::ACME"), epic(2, "beta", "org::GLOBEX")),
	}
	sp := newSourcePipeline(t, nil, nil)
	if _, _, _, err := sp.p.runEpicRollup(context.Background(), projects); err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}

	title := func(req publisher.Request) string {
		return string(publisher.NewFrontmatter(req.Project, req.Audience, req.Period, req.Kind, time.Now(), req.Body, "").Title)
	}
	var byCustomer, byKind string
	for _, w := range sp.pub.written {
		if !strings.Contains(w.Name, "epics-") || strings.Contains(w.Name, "epics-rollup") {
			continue
		}
		switch {
		case strings.Contains(w.Name, slugCustomer("ACME")):
			byCustomer = title(w)
		case strings.Contains(w.Name, slugCustomer("GLOBEX")):
			byKind = title(w)
		}
	}
	if byCustomer == "" || byKind == "" {
		t.Fatalf("expected both customer files; names: %v", publishedNames(sp))
	}
	if byCustomer == byKind {
		t.Errorf("both customer rollups share the frontmatter title %q — the "+
			"docbuilder index cannot tell the two public documents apart", byCustomer)
	}
	if !strings.Contains(byCustomer, "ACME") {
		t.Errorf("ACME's document title does not name ACME: %q", byCustomer)
	}
	if !strings.Contains(tagsOf(sp, slugCustomer("ACME")), "ACME") {
		t.Errorf("ACME's document is not tagged with its customer code")
	}
}

func tagsOf(sp *sourcePipeline, slug string) string {
	for _, w := range sp.pub.written {
		if strings.Contains(w.Name, slug) && !strings.Contains(w.Name, "epics-rollup") {
			return fmt.Sprint(publisher.NewFrontmatter(w.Project, w.Audience, w.Period, w.Kind, time.Now(), w.Body, "").Tags)
		}
	}
	return ""
}

// TestEpicRollup_GroupScopedEpicOnlyAppearsWhereItHasEvents:
// ActiveEpics is the GROUP's list, because epics are group-scoped in
// GitLab — /groups/:id/epics. Every project in the group therefore
// carries the same epics, including projects that own none of the
// epic's issues. Without an event-count filter the epic was listed
// under all of them, on a file published public: true, claiming
// "Activity in window: 0 events".
func TestEpicRollup_GroupScopedEpicOnlyAppearsWhereItHasEvents(t *testing.T) {
	// The epic's issues all live in "a". "b" shares the group, so it
	// sees the epic, but nothing of it happened in "b".
	inA := active(ProjectResult{Project: "a", CustomerOn: true},
		epic(1, "Only in A", "org::ACME"))
	inB := ProjectResult{Project: "b", CustomerOn: true, ActiveEpics: inA.ActiveEpics}
	projects := []ProjectResult{inA, inB}

	for _, b := range bucketAllEpics(projects) {
		for _, ent := range b.Entries {
			for _, s := range ent.Epics {
				if ent.Project == "b" {
					t.Errorf("team rollup: epic %q listed under %s, which owns none of it",
						s.Epic.Title, ent.Project)
				}
			}
		}
	}
	for _, b := range bucketEpicsByCustomer(projects) {
		for _, ent := range b.Entries {
			for _, s := range ent.Epics {
				if ent.Project == "b" {
					t.Errorf("customer rollup: epic %q listed under %s, which owns none of it",
						s.Epic.Title, ent.Project)
				}
			}
		}
	}

	// And it is still there for "a", where it does belong — the fix
	// filters on the count, it does not filter the epic away.
	team := bucketAllEpics(projects)
	if len(team) != 1 || len(team[0].Entries) != 1 || team[0].Entries[0].Project != "a" ||
		len(team[0].Entries[0].Epics) != 1 {
		t.Fatalf("the epic should remain under a: %+v", team)
	}
}

// TestEpicRollup_EpicSkippedWhenThisCustomerHasNoEvents: the
// per-customer filter used to run on ev.Total while the document
// prints ev.ByCustomer[code] — two different numbers.
//
// Two ways a non-zero total says nothing about the customer whose file
// this is. ActiveEpics is the GROUP's list, so the project may own none
// of the epic's issues. And even when it owns some, those issues may be
// another customer's: `codes` comes from EpicCustomerCodes, which reads
// the group-wide issue list, so one GLOBEX-tagged issue anywhere in the
// group opens a GLOBEX bucket from every project.
//
// Measured before the fix, from project g/a:
//
//	Total=1  ByCustomer=map[ACME:1]  EpicCustomerCodes=[ACME GLOBEX]
//	CUST ACME   project=g/a → 1 event(s)
//	CUST GLOBEX project=g/a → 0 event(s)     <- public: true
func TestEpicRollup_EpicSkippedWhenThisCustomerHasNoEvents(t *testing.T) {
	inA := active(ProjectResult{Project: "g/a", CustomerOn: true},
		epic(1, "Billing revamp", "org::ACME"))
	// Same group, same epic, so g/b has the identical pointer — but
	// from g/a's vantage point none of the epic's events are GLOBEX's.
	inA.ActiveEpics[0].Issues = []gitlab.Issue{
		{IID: 10, Labels: []string{"org::GLOBEX"}, References: gitlab.IssueRef{Full: "g/b#10"}},
	}
	projects := []ProjectResult{inA}

	for _, b := range bucketEpicsByCustomer(projects) {
		for _, ent := range b.Entries {
			for _, s := range ent.Epics {
				t.Logf("CUST %-7s project=%s epic %q → %d event(s)",
					b.Customer, ent.Project, s.Epic.Title, s.EventCount)
				if s.EventCount == 0 {
					t.Errorf("customer %s got %q with 0 events on a file published "+
						"public: true", b.Customer, s.Epic.Title)
				}
			}
		}
	}
	if got := bucketEpicsByCustomer(projects); len(got) != 1 || got[0].Customer != "ACME" {
		t.Errorf("want only ACME's bucket, got %d: %+v", len(got), got)
	}
}

// TestEpicRollup_EpicKeptWhenThisCustomerHasEvents: the filter is on
// the per-customer count, so it must not drop an epic the customer
// genuinely worked on just because the total is small.
func TestEpicRollup_EpicKeptWhenThisCustomerHasEvents(t *testing.T) {
	projects := []ProjectResult{
		on("g/a", epic(1, "ACME work", "org::ACME")),
	}
	got := bucketEpicsByCustomer(projects)
	if len(got) != 1 || got[0].Customer != "ACME" {
		t.Fatalf("ACME's own epic was dropped: %+v", got)
	}
	if n := got[0].Entries[0].Epics[0].EventCount; n != 1 {
		t.Errorf("event count = %d, want 1", n)
	}
}

// TestRollup_RecordsItsTokenUsage: Result.TokensByAudience was declared,
// initialised and printed by printSummary with nothing ever writing it —
// runAudienceRollups took resp.Usage from every rollup completion and
// dropped it. So the run summary showed per-project tokens and no total,
// on a tool whose --max-events help says the flag bounds the per-run
// bill, and the rollups are among the largest calls in a run.
func TestRollup_RecordsItsTokenUsage(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{
		{Project: "a", CustomerOn: true, CustomerText: "c", TeamText: "t"},
		{Project: "b", CustomerOn: true, CustomerText: "c", TeamText: "t"},
	}

	_, _, tokens, err := sp.p.runRollup(context.Background(), projects)
	if err != nil {
		t.Fatalf("runRollup: %v", err)
	}
	if len(tokens) == 0 {
		t.Fatal("runRollup reported no token usage; the map is printed by the summary")
	}
	for _, want := range []string{"rollup/customer", "rollup/team"} {
		u, ok := tokens[want]
		if !ok {
			t.Errorf("no usage recorded for %q; got %v", want, keysOf(tokens))
			continue
		}
		t.Logf("tokens[%s]: prompt=%d completion=%d total=%d",
			want, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	}
	// The epic rollup keys separately ("epics-rollup/customer"), so it
	// cannot overwrite the combined rollup's figures for the same
	// audience.
	epic := &gitlab.Epic{IID: 1, Title: "e", Issues: []gitlab.Issue{
		{IID: 1, Labels: []string{"org::ACME"}, References: gitlab.IssueRef{Full: "a#1"}},
	}}
	withEpics := []ProjectResult{{
		Project: "a", CustomerOn: true, CustomerText: "c", TeamText: "t",
		ActiveEpics: []*gitlab.Epic{epic},
		EpicEventCounts: map[int]EpicEvents{
			1: {Total: 1, ByCustomer: map[string]int{"ACME": 1}},
		},
	}}
	_, _, epicTokens, err := sp.p.runEpicRollup(context.Background(), withEpics)
	if err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}
	if _, clash := epicTokens["rollup/customer"]; clash {
		t.Errorf("the epic rollup wrote the combined rollup's key: %v", keysOf(epicTokens))
	}
	found := false
	for _, k := range keysOf(epicTokens) {
		if strings.HasPrefix(k, "epics-rollup/") {
			found = true
		}
	}
	if !found {
		t.Errorf("no epics-rollup/ key in the epic rollup's usage: %v", keysOf(epicTokens))
	}
}

// TestMergeUsage_SumsRatherThanOverwrites: the two rollup paths return
// separate maps that Run folds together. If a key ever collided, an
// overwrite would under-report the bill — the one thing this field
// exists to prevent.
func TestMergeUsage_SumsRatherThanOverwrites(t *testing.T) {
	dst := map[string]llm.Usage{"rollup/customer": {PromptTokens: 10, TotalTokens: 30}}
	mergeUsage(dst, map[string]llm.Usage{
		"rollup/customer": {PromptTokens: 5, TotalTokens: 15},
		"epics/customer":  {PromptTokens: 2, TotalTokens: 4},
	})
	if got := dst["rollup/customer"].TotalTokens; got != 45 {
		t.Errorf("colliding key = %d, want 45 (summed, not overwritten)", got)
	}
	if got := dst["epics/customer"].TotalTokens; got != 4 {
		t.Errorf("new key = %d, want 4", got)
	}
}

func keysOf(m map[string]llm.Usage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEpicRollup_TeamBucketCarriesNoCustomerLabel: the companion to
// TestEpicRollup_PerCustomerFrontmatterNamesTheCustomer, which pins the
// other half of the claim runEpicRollup's comment makes: a per-customer
// bucket carries the customer code, the single team-wide bucket carries
// none.
//
// That second half was asserted nowhere, and the comment that used to
// describe it — "the frontmatter carries Project: \"\"" — was
// half-wrong for several rounds before anyone noticed, because nothing
// tested either half. A claim in a comment about which of two branches
// a field takes is exactly the kind a test can hold and a reader
// cannot.
func TestEpicRollup_TeamBucketCarriesNoCustomerLabel(t *testing.T) {
	sp := newSourcePipeline(t, nil, nil)
	projects := []ProjectResult{
		on("a", epic(1, "alpha", "org::ACME")),
		on("b", epic(2, "beta", "org::ACME")),
	}
	if _, _, _, err := sp.p.runEpicRollup(context.Background(), projects); err != nil {
		t.Fatalf("runEpicRollup: %v", err)
	}

	var team publisher.Request
	found := false
	for _, w := range sp.pub.written {
		if w.Audience != "team" {
			continue
		}
		team, found = w, true
	}
	if !found {
		t.Fatalf("no team rollup published; names: %v", publishedNames(sp))
	}
	if !strings.HasSuffix(team.Name, "--team.md") || !strings.Contains(team.Name, "epics-rollup") {
		t.Fatalf("expected the single combined team file, got %q", team.Name)
	}
	if team.Project != "" {
		t.Errorf("the team-wide bucket carried Project=%q; it spans every "+
			"project, so it must name none — and NewFrontmatter turns a "+
			"non-empty value here into a title that reads as one project", team.Project)
	}
	// And it must still be titled as the fleet, not as ACME: the two
	// epics here are ACME's, but the file is internal and spans
	// everything.
	title := fmt.Sprint(publisher.NewFrontmatter(team.Project, team.Audience, team.Period, team.Kind,
		time.Now(), team.Body, "").Title)
	if strings.Contains(title, "ACME") {
		t.Errorf("the combined team rollup is titled %q, as if it were ACME's", title)
	}
	if !strings.Contains(title, "all projects") {
		t.Errorf("team rollup title = %q, want it to say it spans every project", title)
	}
}
