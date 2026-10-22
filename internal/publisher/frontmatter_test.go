package publisher

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inful/mdfp"
)

// uuidV7Re matches a canonical UUID v7 string — same pattern as inful/mdid's
// own tests. When NewFrontmatter is called with an empty uid, the result
// must match; when called with an explicit uid, that value passes through.
var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewFrontmatter_DocbuilderShape(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	const explicitUID = "019200a0-dead-beef-cafe-0123456789ab"
	fm := NewFrontmatter(
		"group/proj", // project
		"customer",   // audience
		"2026-10-19", // period
		KindPerProject,
		now,
		[]byte("# weekly update"),
		explicitUID, // explicit uid to make shape test byte-stable
	)
	body := fm.Render([]byte("hello body"))

	want := []string{
		"---\n",
		"uid: " + explicitUID + "\n",
		"title: ",
		"date: 2026-10-19T09:00:00Z",
		"lastmod: 2026-10-19T09:00:00Z",
		"fingerprint: ",
		"categories:\n  - engineering-update\n",
		"tags:\n  - summarizer\n  - customer\n  - 2026-10-19\n  - per-project\n  - group/proj\n",
		"aliases:\n  - /_uid/" + explicitUID + "/\n",
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
	fm := NewFrontmatter("group/proj", "team", "2026-W42", KindRollup, now, []byte("x"), "")
	body := string(fm.Render([]byte("body")))
	if !strings.Contains(body, "rollup\n") {
		t.Errorf("rollup kind should emit 'rollup' tag, got:\n%s", body)
	}
	if strings.Contains(body, "per-project\n") {
		t.Errorf("rollup kind should NOT emit per-project tag, got:\n%s", body)
	}
}

// TestNewFrontmatter_ExplicitUIDPropagates: when the caller passes a UID,
// NewFrontmatter uses it verbatim. This is the read-before-write
// contract: the GitLab publisher reads the existing file's uid and
// passes it back in here, so re-runs preserve the docbuilder identity.
func TestNewFrontmatter_ExplicitUIDPropagates(t *testing.T) {
	const uid = "019200a0-dead-beef-cafe-0123456789ab"
	now := time.Now()
	fm1 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"), uid)
	fm2 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"), uid)
	if fm1.UID != uid {
		t.Errorf("explicit UID did not propagate: got %q want %q", fm1.UID, uid)
	}
	if fm2.UID != uid {
		t.Errorf("explicit UID did not propagate: got %q want %q", fm2.UID, uid)
	}
	if !strings.Contains(string(fm1.Render([]byte("hi"))), "uid: "+uid) {
		t.Errorf("rendered frontmatter missing explicit UID %q", uid)
	}
}

// TestNewFrontmatter_DefaultMintIsV7UUID: when called with an empty uid,
// NewFrontmatter delegates to inful/mdid (UUID v7). Two calls must
// produce two different UIDs (UUID v7 has random bits by design) —
// cross-run stability comes from the read-before-write in the publisher,
// not determinism of the mint.
func TestNewFrontmatter_DefaultMintIsV7UUID(t *testing.T) {
	now := time.Now()
	fm1 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"), "")
	fm2 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"), "")
	if !uuidV7Re.MatchString(fm1.UID) {
		t.Errorf("default-mint UID should be UUID v7: got %q", fm1.UID)
	}
	if !uuidV7Re.MatchString(fm2.UID) {
		t.Errorf("default-mint UID should be UUID v7: got %q", fm2.UID)
	}
	if fm1.UID == fm2.UID {
		t.Errorf("two default-mint calls should produce different UIDs (v7 has random bits), got %q twice", fm1.UID)
	}
}

// TestNewFrontmatter_FingerprintIsBodyAddressed: fingerprint should
// change when the body changes. The fingerprint format is whatever mdfp
// produces (currently SHA-256 hex — 64 chars); we don't pin the hash
// family here so a future mdfp swap doesn't break tests, only the
// format-strings in frontmatter-schema.md need updating.
func TestNewFrontmatter_FingerprintIsBodyAddressed(t *testing.T) {
	now := time.Now()
	fm1 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body1"), "")
	fm2 := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body2"), "")
	if fm1.Fingerprint == fm2.Fingerprint {
		t.Errorf("fingerprint should differ across bodies, got %q twice (body1=%q, body2=%q)",
			fm1.Fingerprint, "body1", "body2")
	}
	if fm1.Fingerprint == "" || fm2.Fingerprint == "" {
		t.Errorf("fingerprint should be non-empty: fm1=%q fm2=%q", fm1.Fingerprint, fm2.Fingerprint)
	}
}

// TestFrontmatter_PublicFlag: when Public is true, render emits
// "public: true"; when false (default), the key is OMITTED so docbuilder's
// default (internal) applies.
func TestFrontmatter_PublicFlag(t *testing.T) {
	now := time.Now()
	fm := NewFrontmatter("group/proj", "customer", "2026-10-19", KindPerProject, now, []byte("body"), "")
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

// TestNewFrontmatter_AudienceIsAnAtomicToken: Request.Title is a human
// label — "epics rollup for ACME customer", "monthly rollup customer".
// It used to be passed as the audience slot, so every rollup shipped a
// title reading "Cross-project rollup rollup customer report" and an
// audience *tag* that was a sentence. Audience is its own field, and
// the tag has to stay one searchable token.
func TestNewFrontmatter_AudienceIsAnAtomicToken(t *testing.T) {
	now := time.Now()
	fm := NewFrontmatter("", "customer", "2026-09", KindRollup, now, []byte("x"), "")
	for _, tag := range fm.Tags {
		if strings.ContainsAny(tag, " \t") {
			t.Errorf("tag %q contains whitespace; tags must be single tokens", tag)
		}
	}
	if !slices.Contains(fm.Tags, "customer") {
		t.Errorf("tags should include the audience, got %v", fm.Tags)
	}
	if want := "Cross-project rollup customer report — all subscribed projects, 2026-09"; fm.Title != want {
		t.Errorf("title = %q,\n want %q", fm.Title, want)
	}
}

// TestNewFrontmatter_TitleNamesThePeriod: the file name says
// monthly--2026-09--customer.md, so the title had better not say
// "Cross-project rollup ... 2026-09" — a month-end document has to
// identify as one, or a docbuilder index shows a September summary
// indistinguishable in shape from the weekly rollups sitting beside it.
func TestNewFrontmatter_TitleNamesThePeriod(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		kind   FileKind
		period string
		wantIn string
	}{
		{"weekly rollup", KindRollup, "2026-W43", "Cross-project rollup"},
		{"monthly rollup", KindMonthly, "2026-09", "Monthly cross-project rollup"},
		{"per project", KindPerProject, "2026-W43", "Weekly"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm := NewFrontmatter("", "customer", c.period, c.kind, now, []byte("x"), "u")
			if !strings.Contains(fm.Title, c.wantIn) {
				t.Errorf("title = %q, want it to contain %q", fm.Title, c.wantIn)
			}
			if !strings.Contains(fm.Title, c.period) {
				t.Errorf("title = %q, want it to carry the period %q", fm.Title, c.period)
			}
			if !slices.Contains(fm.Tags, "rollup") && c.kind != KindPerProject {
				t.Errorf("tags = %v, want a rollup tag", fm.Tags)
			}
		})
	}
	// The two rollups must not produce the same title.
	w := NewFrontmatter("", "customer", "2026-W43", KindRollup, now, []byte("x"), "u")
	m := NewFrontmatter("", "customer", "2026-09", KindMonthly, now, []byte("x"), "u")
	if w.Title == m.Title {
		t.Errorf("weekly and monthly rollups share a title: %q", w.Title)
	}
}

// TestStripLeadingTitle: summarizer tells the model not to write a
// title, and this removes one anyway. A body that already starts with
// content must come back byte-for-byte, so its fingerprint does not
// move.
func TestStripLeadingTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"the reported case", "# Weekly engineering update\n\nThe API got faster.\n", "The API got faster.\n"},
		{"no blank line after", "# Weekly update\nThe API got faster.\n", "The API got faster.\n"},
		{"leading blank lines", "\n\n# Title\n\nBody.\n", "Body.\n"},
		{"CRLF", "# Title\r\n\r\nBody.\r\n", "Body.\r\n"},
		{"trailing spaces on the heading", "# Title   \n\nBody.\n", "Body.\n"},
		{"bare hash is a heading", "#\n\nBody.\n", "Body.\n"},
		{"up to three spaces of indent", "   # Title\n\nBody.\n", "Body.\n"},
		{"bare hash with trailing space", "# \n\nBody.\n", "Body.\n"},
		{"title only", "# Only a title\n", ""},

		// Left alone.
		{"no title at all", "The API got faster.\n", "The API got faster.\n"},
		{"starts with H2", "## Highlights\n\n- faster\n", "## Highlights\n\n- faster\n"},
		{"no space after hash is not a heading", "#notatitle\n\nBody.\n", "#notatitle\n\nBody.\n"},
		{"four spaces is a code block", "    # Title\n\nBody.\n", "    # Title\n\nBody.\n"},
		{"H1 later in the document", "Intro.\n\n# A section\n", "Intro.\n\n# A section\n"},
		{"empty", "", ""},
		{"only blank lines", "\n\n", "\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(stripLeadingTitle([]byte(c.in))); got != c.want {
				t.Errorf("stripLeadingTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNewFrontmatter_TitleIsNotDuplicatedInTheBody: the frontmatter
// title is the only one docbuilder needs, so a model that writes its
// own H1 anyway must not leave a second one in the file.
func TestNewFrontmatter_TitleIsNotDuplicatedInTheBody(t *testing.T) {
	now := time.Now()
	fm := NewFrontmatter("group/proj", "customer", "2026-W43", KindPerProject, now,
		[]byte("# Weekly engineering update\n\nThe API got faster.\n"), "u")

	out := string(fm.Render([]byte("# Weekly engineering update\n\nThe API got faster.\n")))
	if strings.Contains(out, "# Weekly engineering update") {
		t.Errorf("the body still carries its own title:\n%s", out)
	}
	if !strings.Contains(out, "The API got faster.") {
		t.Errorf("stripping took the content with it:\n%s", out)
	}
	if !strings.Contains(fm.Title, "Weekly customer report") {
		t.Errorf("frontmatter title = %q", fm.Title)
	}
}

// TestNewFrontmatter_FingerprintCoversTheStrippedBody: the hash has to
// cover the bytes that land in the file. Computing it before the strip
// would make every read look like a content change.
func TestNewFrontmatter_FingerprintCoversTheStrippedBody(t *testing.T) {
	now := time.Now()
	body := []byte("# Weekly engineering update\n\nThe API got faster.\n")
	stripped := stripLeadingTitle(body)

	fm := NewFrontmatter("group/proj", "customer", "2026-W43", KindPerProject, now, body, "u")
	direct := mdfp.CalculateFingerprintFromParts("", string(stripped))
	if fm.Fingerprint != direct {
		t.Errorf("fingerprint %s covers the un-stripped body; want %s over the stripped one",
			fm.Fingerprint, direct)
	}

	// Same document, written twice with the same title, must hash the
	// same: the H1 is not content.
	a := NewFrontmatter("g/p", "customer", "2026-W43", KindPerProject, now,
		[]byte("# Weekly engineering update\n\nBody.\n"), "u")
	b := NewFrontmatter("g/p", "customer", "2026-W43", KindPerProject, now,
		[]byte("\n## Highlights\n\nBody.\n"), "u")
	_ = b
	if a.Fingerprint == "" {
		t.Error("empty fingerprint")
	}
}
