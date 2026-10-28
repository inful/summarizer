package publisher

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLocalPublisher_WritesItsOwnTitle: v0.3.0 taught all eight
// templates not to write a title, and the local-disk publisher writes
// no frontmatter to carry one — so a local report had NO title at all,
// starting straight at "## Highlights".
//
// Worse, it was not even consistently untitled: the local publisher
// writes req.Body raw, so an H1 the model wrote survived. The file was
// titled or untitled depending on whether the model obeyed the
// instruction.
//
// The local publisher now supplies the heading itself, using the same
// publisher.Title the frontmatter would have carried, so a local file
// and a published file with the same inputs are titled identically.
func TestLocalPublisher_WritesItsOwnTitle(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)

	// A body with NO heading, as the prompts now ask for.
	res, err := lp.Publish(context.Background(), Request{
		Name: "g@p--wk--customer.md", Period: "2026-W42",
		Body:     []byte("## Highlights\n\n- Shipped the exporter.\n"),
		Project:  "g/p",
		Audience: "customer",
		Kind:     KindPerProject,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(res.Ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "# " + Title("g/p", "customer", "2026-W42", KindPerProject) +
		"\n\n## Highlights\n\n- Shipped the exporter.\n"
	if string(got) != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A model that disobeys the instruction must not produce a file with
// two headings: the publisher's own title replaces the model's.
func TestLocalPublisher_ModelHeadingIsReplacedNotAppended(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)

	res, err := lp.Publish(context.Background(), Request{
		Name: "g@p--wk--team.md", Period: "2026-W42",
		Body:     []byte("# Weekly engineering update\n\nbody\n"),
		Project:  "g/p",
		Audience: "team",
		Kind:     KindPerProject,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := os.ReadFile(res.Ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(got), "\n# "); n != 0 {
		t.Errorf("the model's own H1 survived alongside the publisher's:\n%s", got)
	}
	if !strings.HasPrefix(string(got), "# "+Title("g/p", "team", "2026-W42", KindPerProject)) {
		t.Errorf("the file does not start with the publisher's title:\n%s", got)
	}
}

// The two sinks must title a document the same way. This is the point
// of extracting publisher.Title: two copies of a title would drift.
func TestTitle_MatchesTheFrontmatterTitle(t *testing.T) {
	for _, c := range []struct {
		project, audience, period string
		kind                      FileKind
	}{
		{"g/p", "customer", "2026-W42", KindPerProject},
		{"g/p", "team", "2026-W42", KindPerProject},
		{"", "customer", "2026-W42", KindRollup},
		{"", "team", "2026-W42", KindRollup},
		{"ACME", "customer", "2026-W42", KindRollup},
		{"", "customer", "2026-09", KindMonthly},
	} {
		fm := NewFrontmatter(c.project, c.audience, c.period, c.kind, time.Now(), []byte("body"), "uid")
		if got := Title(c.project, c.audience, c.period, c.kind); got != fm.Title {
			t.Errorf("Title(%q,%q,%q,%v) = %q but the frontmatter says %q",
				c.project, c.audience, c.period, c.kind, got, fm.Title)
		}
	}
}
