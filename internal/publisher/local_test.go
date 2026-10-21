package publisher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocal_OverwritesInPlace: refuse-first is gone. Every run inside a
// period targets the same (project, audience, period) path, so a second
// run has to overwrite — otherwise the daily cron would fail on the
// second day of every week.
func TestLocal_OverwritesInPlace(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	req := Request{Name: "a.md", Period: "2026-W42", Body: []byte("first")}

	if _, err := l.Publish(req); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	req.Body = []byte("second")
	res, err := l.Publish(req)
	if err != nil {
		t.Fatalf("re-publish in the same period: %v", err)
	}
	if res.Ref == "" {
		t.Error("Ref must name the written file")
	}
	got, _ := os.ReadFile(res.Ref)
	if string(got) != "second" {
		t.Errorf("re-publish did not overwrite: %q", got)
	}
}

// TestLocal_NoTempFilesLeftBehind: every failure path must clean up its
// temp file. A cron that dies on project 7 of 12 should not leave
// .a.md.tmp-XXXX litter in the output directory forever.
func TestLocal_NoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	res, err := l.Publish(Request{Name: "a.md", Period: "2026-W42", Body: []byte("x")})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(res.Ref))
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

// TestLocal_CreatesMissingDirectories: output_dir may not exist yet,
// and neither may the period subdirectory under it.
func TestLocal_CreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "out")
	l := NewLocal(dir)
	res, err := l.Publish(Request{Name: "a.md", Period: "2026-W42", Body: []byte("x")})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, statErr := os.Stat(res.Ref); statErr != nil {
		t.Errorf("file not at %s: %v", res.Ref, statErr)
	}
}
