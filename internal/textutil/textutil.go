// Package textutil holds the string helpers shared across summarizer's
// packages. One home so a fix (rune-safety, cap semantics) lands once
// instead of once per copy.
package textutil

import "unicode/utf8"

// Truncate cuts s to at most n RUNES and appends an ellipsis marker
// when it cut anything. Rune-based, not byte-based: byte slicing splits
// multi-byte characters and leaves an invalid UTF-8 lead byte in the
// output, which then travels into an LLM prompt. Titles and descriptions
// in a Nordic or mixed-language repo hit this at the cap boundary.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
