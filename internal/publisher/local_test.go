package publisher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocal_RefusesExistingFile: refuse-first is the default contract.
// Without this the daily cron would silently overwrite yesterday's
// report if the period bucket repeated.
func TestLocal_RefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	req := Request{Name: "a.md", Body: []byte("first")}

	if _, err := l.Publish(req); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := l.Publish(req)
	if err == nil {
		t.Fatal("second publish should have been refused")
	}
	if !errors.Is(err, ErrExists) {
		t.Errorf("want ErrExists, got %v", err)
	}
	// The original content must survive the refusal.
	got, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	if string(got) != "first" {
		t.Errorf("refused publish overwrote the file: %q", got)
	}
}

// TestLocal_ForceOverwrites: --force and monday-mode implied force both
// land here.
func TestLocal_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	if _, err := l.Publish(Request{Name: "a.md", Body: []byte("first")}); err != nil {
		t.Fatalf("first: %v", err)
	}
	req := Request{Name: "a.md", Body: []byte("second"), Force: true}
	res, err := l.Publish(req)
	if err != nil {
		t.Fatalf("forced publish: %v", err)
	}
	if res.Skipped {
		t.Error("Skipped should be false on a forced write")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.md"))
	if string(got) != "second" {
		t.Errorf("force did not overwrite: %q", got)
	}
}

// TestLocal_NoTempFilesLeftBehind: every failure path must clean up its
// temp file. A cron that dies on project 7 of 12 should not leave
// .a.md.tmp-XXXX litter in the output directory forever.
func TestLocal_NoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	if _, err := l.Publish(Request{Name: "a.md", Body: []byte("x")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("want exactly one file, got %v", names)
	}
}

// TestLocal_CreatesMissingDirectories: output_dir may not exist yet.
func TestLocal_CreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "out")
	l := NewLocal(dir)
	res, err := l.Publish(Request{Name: "a.md", Body: []byte("x")})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, statErr := os.Stat(res.Ref); statErr != nil {
		t.Errorf("file not at %s: %v", res.Ref, statErr)
	}
}

// TestLocal_RefusedResultStillCarriesRef: a cron summary prints the Ref
// so the operator can find the file that was already there.
func TestLocal_RefusedResultStillCarriesRef(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	req := Request{Name: "a.md", Body: []byte("x")}
	if _, err := l.Publish(req); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := l.Publish(req)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !res.Skipped {
		t.Error("Skipped should be true on refusal")
	}
	if res.Ref == "" {
		t.Error("refused result should still name the existing file")
	}
}
