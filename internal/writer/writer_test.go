package writer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_AtomicSuccess(t *testing.T) {
	dir := t.TempDir()
	path, err := New().Write(dir, "report.md", []byte("hello"), false)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content: %q", got)
	}
	// No leftover tmp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".md" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestWrite_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	w := New()
	if _, err := w.Write(dir, "r.md", []byte("first"), false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err := w.Write(dir, "r.md", []byte("second"), false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "r.md"))
	if string(got) != "first" {
		t.Fatalf("refused write should leave original, got %q", got)
	}
}

func TestWrite_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	w := New()
	if _, err := w.Write(dir, "r.md", []byte("first"), false); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := w.Write(dir, "r.md", []byte("second"), true); err != nil {
		t.Fatalf("force: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "r.md"))
	if string(got) != "second" {
		t.Fatalf("force should overwrite, got %q", got)
	}
}