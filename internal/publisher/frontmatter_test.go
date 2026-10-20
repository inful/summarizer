package publisher

import (
	"strings"
	"testing"
	"time"
)

func TestNewFrontmatter_DocbuilderShape(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	fm := NewFrontmatter(
		"group/proj",     // project
		"customer",       // audience
		"2026-10-19",     // period
		KindPerProject,
		now,
		[]byte("# weekly update"),
	)
	body := fm.Render([]byte("hello body"))

	want := []string{
		"---\n",
		"uid: ",
		"title: ",
		"date: 2026-10-19T09:00:00Z",
		"lastmod: 2026-10-19T09:00:00Z",
		"fingerprint: ",
		"categories:\n  - engineering-update\n",
		"tags:\n  - summarizer\n  - customer\n  - 2026-10-19\n  - per-project\n  - group/proj\n",
		"aliases:\n  - /_uid/",
		"\nhello body",
	}
	for _, w := range want {
		if !strings.Contains(string(body), w) {
			t.Errorf("frontmatter should contain %q, got:\n%s", w, body)
		}
	}
}

func TestNewFrontmatter_RollupExtraTag(t *testing.T) {
	now := time.Now()
	fm := NewFrontmatter("group/proj", "team", "2026-W42", KindRollup, now, []byte("x"))
	body := string(fm.Render([]byte("body")))
	if !strings.Contains(body, "rollup\n") {
		t.Errorf("rollup kind should emit 'rollup' tag, got:\n%s", body)
	}
	if strings.Contains(body, "per-project\n") {
		t.Errorf("rollup kind should NOT emit per-project tag, got:\n%s", body)
	}
}

func TestNewFrontmatter_UIDIsDeterministic(t *testing.T) {
	now := time.Now()
	fm1 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"))
	fm2 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"))
	if fm1.UID != fm2.UID {
		t.Errorf("UID should be deterministic for same inputs: %q vs %q", fm1.UID, fm2.UID)
	}
	fm3 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("different body"))
	if fm1.Fingerprint == fm3.Fingerprint {
		t.Errorf("fingerprint should change with body content: %q", fm1.Fingerprint)
	}
}

// TestFrontmatter_PublicFlag: when Public is true, render emits
// "public: true"; when false (default), the key is OMITTED so docbuilder's
// default (internal) applies.
func TestFrontmatter_PublicFlag(t *testing.T) {
	now := time.Now()
	fm := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"))
	body := string(fm.Render([]byte("hi")))

	// Default: key absent.
	if strings.Contains(body, "public:") {
		t.Errorf("Public false should not emit any 'public:' line, got:\n%s", body)
	}

	// Set true: line present.
	fm.Public = true
	body = string(fm.Render([]byte("hi")))
	if !strings.Contains(body, "public: true") {
		t.Errorf("Public true should emit 'public: true', got:\n%s", body)
	}
}
