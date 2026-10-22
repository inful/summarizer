package prompt

import (
	"fmt"
	"strings"
	"testing"
)

// unsafeFields are the template interpolations whose value a GitLab user
// controls: an MR/epic title, a project path, a rolled-up body. Each
// must sit inside a <data> block and go through the `data` helper —
// the block alone is not enough, because a value containing "</data>"
// closes the block early and the rest of it lands in the region the
// system prompt calls trusted.
//
// Everything else the templates touch is safe by construction: an int
// count, a GitLab enum (.Epic.State), a server-generated URL
// (.Epic.WebURL), a tool-generated slug ($week), or .Events, which
// writeEventLine escapes field by field as it builds the blob. List them
// all with: grep -oh '{{[^}]*}}' internal/prompt/templates/*.tmpl
var unsafeFields = map[string]bool{
	".Title":      true, // MR / issue / epic title
	".Epic.Title": true, // epic title in the rollups
	".Project":    true, // project path — a maintainer can rename it
	"$name":       true, // .ProjectNames — project paths again
	"$body":       true, // rolled-up week / project body
	".":           true, // the single-variable form: {{data .}} over .ProjectNames
	// "." is the single-variable form of the same thing: `{{data .}}`
	// over .ProjectNames. It was MISSING here, so downgrading that to a
	// bare `{{.}}` in rollup-team.tmpl passed this test — in a template
	// published public: true, where a project path containing "</data>"
	// would escape the block. The monthly templates also ranged over a
	// list with `.`, but theirs are week slugs and safe; they now use a
	// named `$week :=` so `.` means one thing and one thing only.
}

// safeFields is the other half of the classification: every template
// interpolation must appear in exactly one of these two maps.
// TestTemplates_EveryFieldIsClassified fails on an unclassified field,
// so "add a field, remember the list" stops being a convention and
// becomes a compile-time-shaped requirement.
//
// Each entry says why the value cannot carry instructions.
var safeFields = map[string]string{
	"$week":         "weekSlugFor(time.Time) — always YYYY-Www, built from an int year and week",
	".Window":       "WeekWindowLabel, a package-level const",
	".Month":        "parseMonth validated it with time.Parse, layout 2006-01",
	".ProjectCount": "an int",
	".WeekCount":    "an int",
	".DroppedCount": "an int",
	".Dropped":      "an int",
	".Used":         "an int",
	".Total":        "an int",
	".ShownCount":   "an int",
	".EventCount":   "an int",
	".Epic.IID":     "an int",
	".Epic.State":   "a GitLab enum: opened / closed / started",
	".Epic.WebURL":  "server-generated from the project path and IID; GitLab paths are [A-Za-z0-9_.-]",
	".Events":       "writeEventLine escapes every field as it builds the blob",
	"len":           "the template builtin; yields an int",
}

func TestTemplates_UntrustedFieldsAreEscaped(t *testing.T) {
	files, err := templatesFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, f := range files {
		name := f.Name()
		raw, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		inData := false
		for i, line := range strings.Split(string(raw), "\n") {
			for _, ref := range templateFields(line) {
				if !unsafeFields[ref.field] {
					continue
				}
				// Two independent faults, and escaping fixes only one:
				// the helper stops a value closing the block, but a
				// value sitting outside the block is still read as
				// instruction regardless of how it is encoded.
				if !inData {
					t.Errorf("%s:%d: {{%s}} is outside <data>, in the region "+
						"the system prompt tells the model to trust",
						name, i+1, ref.field)
					continue
				}
				if !ref.escaped {
					t.Errorf("%s:%d: {{%s}} is a bare interpolation inside "+
						"<data>; write {{data %s}} so a value containing "+
						"\"</data>\" cannot close the block",
						name, i+1, ref.field, ref.field)
				}
			}
			// A line that is exactly the tag opens or closes the block.
			// Prose mentioning "<data>" (the system prompt does) must not.
			switch strings.TrimSpace(line) {
			case "<data>":
				inData = true
			case "</data>":
				inData = false
			}
		}
	}
}

// templateRef is one {{ … }} on a line that names a value rather than
// control flow.
type templateRef struct {
	field   string
	escaped bool // went through the `data` helper
}

// templateFields returns the value interpolations on one line, skipping
// directives (if/range/end). Brace matching is a fixed scan: the
// templates have no nested braces inside an interpolation, so this is
// enough to find bare fields.
func templateFields(line string) []templateRef {
	directives := map[string]bool{
		"if": true, "else": true, "range": true, "end": true,
		"with": true, "block": true, "define": true,
	}
	var out []templateRef
	for i := 0; i < len(line); i++ {
		if i+1 >= len(line) || line[i] != '{' || line[i+1] != '{' {
			continue
		}
		j := strings.Index(line[i+2:], "}}")
		if j < 0 {
			continue
		}
		parts := strings.Fields(strings.TrimSpace(line[i+2 : i+2+j]))
		if len(parts) > 0 && directives[parts[0]] {
			i += 2 + j + 1
			continue
		}
		if len(parts) == 2 && parts[0] == "data" {
			out = append(out, templateRef{field: parts[1], escaped: true})
		} else if len(parts) > 0 {
			out = append(out, templateRef{field: parts[0]})
		}
		i += 2 + j + 1
	}
	return out
}

// TestTemplates_EveryFieldIsClassified: the escaping test above can
// only check fields someone remembered to list. A new interpolation,
// or one whose field name changes, passes silently — which is exactly
// how `.` stayed out of unsafeFields while `{{data .}}` sat in two
// `public: true` templates: downgrading it to a bare `{{.}}` passed the
// suite.
//
// Every field must land in exactly one of the two maps, and the maps
// must not overlap. Adding a field to a template without classifying it
// fails here.
func TestTemplates_EveryFieldIsClassified(t *testing.T) {
	for field := range safeFields {
		if unsafeFields[field] {
			t.Errorf("%s is in both maps: it cannot be both safe and unsafe", field)
		}
	}

	files, err := templatesFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	seen := map[string]string{} // field -> where it was first seen
	for _, f := range files {
		raw, err := templatesFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatalf("read %s: %v", f.Name(), err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, ref := range templateFields(line) {
				if unsafeFields[ref.field] || safeFields[ref.field] != "" {
					continue
				}
				where := fmt.Sprintf("%s:%d", f.Name(), i+1)
				if prev, dup := seen[ref.field]; dup {
					t.Errorf("{{%s}} is unclassified (first seen %s, now %s); add it to "+
						"unsafeFields or safeFields", ref.field, prev, where)
					continue
				}
				seen[ref.field] = where
				t.Errorf("{{%s}} at %s is in neither map; unsafeFields if a GitLab "+
					"user controls it, safeFields (with a reason) if not", ref.field, where)
			}
		}
	}
	if t.Failed() {
		t.Logf("re-derive the field list with: grep -oh '{{[^}]*}}' internal/prompt/templates/*.tmpl")
	}
}
