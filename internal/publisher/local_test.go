package publisher

import (
	"context"
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

	if _, err := l.Publish(context.Background(), req); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	req.Body = []byte("second")
	res, err := l.Publish(context.Background(), req)
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
	res, err := l.Publish(context.Background(), Request{Name: "a.md", Period: "2026-W42", Body: []byte("x")})
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
	res, err := l.Publish(context.Background(), Request{Name: "a.md", Period: "2026-W42", Body: []byte("x")})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, statErr := os.Stat(res.Ref); statErr != nil {
		t.Errorf("file not at %s: %v", res.Ref, statErr)
	}
}

// TestLocal_CancelledRunLeavesThePreviousFileIntact: the writer took a
// ctx and never read it, so Ctrl-C mid-write went unnoticed and the run
// reported success for a file nobody asked it to finish. The check sits
// immediately before the rename — the last moment the previous period's
// file is still intact — so a cancelled run must leave it untouched and
// must not leave a temp file behind.
func TestLocal_CancelledRunLeavesThePreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	req := Request{Name: "proj--wk--team.md", Body: []byte("first run"), Period: "2026-W43"}

	if _, err := l.Publish(context.Background(), req); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	target := filepath.Join(dir, "2026-W43", "proj--wk--team.md")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req.Body = []byte("second run, cancelled")
	if _, err := l.Publish(ctx, req); err == nil {
		t.Fatal("a cancelled context must not report success")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("previous file should still be readable: %v", err)
	}
	if string(got) != "first run" {
		t.Errorf("target = %q, want the previous run's content left intact", got)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "2026-W43"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("cancelled run left a temp file behind: %s", e.Name())
		}
	}
}
