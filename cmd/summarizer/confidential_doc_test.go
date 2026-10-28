package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDocs_ConfidentialExclusionIsDocumented: v0.3.0 shipped the fix —
// confidential issues and epics no longer reach customer-facing output —
// and the guarantee was documented NOWHERE outside the source and the
// changelog. `grep -rni "confidential" README.md docs/` returned
// nothing.
//
// An operator asking "does my confidential work end up in the
// customer's file?" had no answer in the docs and had to read the
// source to find one. A security guarantee nobody can discover is a
// guarantee nobody relies on.
//
// This is deliberately a presence check, not a wording check: the point
// is that the question is ANSWERED somewhere an operator reads, not
// that it is answered in any particular way.
func TestDocs_ConfidentialExclusionIsDocumented(t *testing.T) {
	for _, doc := range []struct {
		path string
		want []string
	}{
		{filepath.Join("..", "..", "README.md"), []string{"confidential", "customer"}},
		{filepath.Join("..", "..", "docs", "runbook.md"), []string{"confidential", "customer"}},
	} {
		text := strings.ToLower(readDoc(t, doc.path))
		for _, w := range doc.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s does not mention %q — the confidentiality exclusion is "+
					"an operator-facing guarantee and must be discoverable in the docs",
					doc.path, w)
			}
		}
	}
}
