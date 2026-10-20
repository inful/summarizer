package main

import (
	"flag"
	"strings"
	"testing"
)

// TestStringSliceFlag_RepeatedFlag covers the --project X --project Y shape.
func TestStringSliceFlag_RepeatedFlag(t *testing.T) {
	var s stringSliceFlag
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&s, "project", "")
	if err := fs.Parse([]string{"--project", "foo/bar", "--project", "baz/qux"}); err != nil {
		t.Fatal(err)
	}
	if got := []string(s); len(got) != 2 || got[0] != "foo/bar" || got[1] != "baz/qux" {
		t.Errorf("repeated flag: got %v, want [foo/bar baz/qux]", got)
	}
}

// TestStringSliceFlag_CommaSeparated covers the --project X,Y shape in a
// single invocation.
func TestStringSliceFlag_CommaSeparated(t *testing.T) {
	var s stringSliceFlag
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&s, "project", "")
	if err := fs.Parse([]string{"--project", "foo/bar,baz/qux"}); err != nil {
		t.Fatal(err)
	}
	if got := []string(s); len(got) != 2 || got[0] != "foo/bar" || got[1] != "baz/qux" {
		t.Errorf("comma-separated: got %v, want [foo/bar baz/qux]", got)
	}
}

// TestStringSliceFlag_Mixed covers repeated + comma-separated in the same
// invocation. Order is preserved per call.
func TestStringSliceFlag_Mixed(t *testing.T) {
	var s stringSliceFlag
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&s, "project", "")
	if err := fs.Parse([]string{
		"--project", "a/b,c/d",
		"--project", "e/f",
		"--project", "g/h,i/j",
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a/b", "c/d", "e/f", "g/h", "i/j"}
	got := []string(s)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestStringSliceFlag_EmptyEntriesDropped covers the defensive trim/empty
// behavior so "--project a,,b" doesn't produce a phantom "" entry.
func TestStringSliceFlag_EmptyEntriesDropped(t *testing.T) {
	var s stringSliceFlag
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&s, "project", "")
	if err := fs.Parse([]string{"--project", "a/b,,c/d,"}); err != nil {
		t.Fatal(err)
	}
	if got := []string(s); len(got) != 2 || got[0] != "a/b" || got[1] != "c/d" {
		t.Errorf("got %v, want [a/b c/d]", got)
	}
}

// TestStringSliceFlag_StringOutput covers the value-rendering helper used
// by `flag.PrintDefaults` and other introspection paths.
func TestStringSliceFlag_StringOutput(t *testing.T) {
	var s stringSliceFlag = []string{"a", "b", "c"}
	if got := s.String(); got != "a,b,c" {
		t.Errorf("String() = %q, want %q", got, "a,b,c")
	}
	var empty stringSliceFlag
	if got := empty.String(); got != "" {
		t.Errorf("empty String() = %q, want \"\"", got)
	}
}

// Reference strings.Join via a tiny helper to keep the test honest about
// the separator without importing strings everywhere; this is here so
// future readers know the String() output shape is intentional.
var _ = strings.Join