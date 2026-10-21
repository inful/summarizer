package publisher

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocalPublisher_PeriodSubdirectory: each period gets its own
// subdirectory, matching what the GitLab publisher does with
// <prefix>/<period>/<name>. Without this, two weeks of the same
// project produce the same filename and the second week overwrites the
// first — local mode is the default (publish.enabled defaults false),
// so this was silent data loss for anyone not on the GitLab publisher.
func TestLocalPublisher_PeriodSubdirectory(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)

	w1, err := lp.Publish(Request{
		Name:   "group-proj--7d--customer.md",
		Period: "2026-W42",
		Body:   []byte("week 42"),
		Force:  true,
	})
	if err != nil {
		t.Fatalf("publish W42: %v", err)
	}
	w2, err := lp.Publish(Request{
		Name:   "group-proj--7d--customer.md",
		Period: "2026-W43",
		Body:   []byte("week 43"),
		Force:  true,
	})
	if err != nil {
		t.Fatalf("publish W43: %v", err)
	}

	if w1.Ref == w2.Ref {
		t.Fatalf("both periods wrote to %s; the second overwrote the first", w1.Ref)
	}
	for _, c := range []struct{ ref, want string }{
		{w1.Ref, "week 42"},
		{w2.Ref, "week 43"},
	} {
		got, err := os.ReadFile(c.ref)
		if err != nil {
			t.Fatalf("read %s: %v", c.ref, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.ref, got, c.want)
		}
	}
	// Both files must still be on disk after both writes.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected 2 period directories under %s, got %v", dir, names)
	}
}

// TestLocalPublisher_SamePeriodOverwrites: within one period, a re-run
// refines the same file in place. That is the whole point of weekly
// buckets — seven daily runs collapse onto one path.
func TestLocalPublisher_SamePeriodOverwrites(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)
	req := Request{Name: "combined--rollup--7d--team.md", Period: "2026-W42", Force: true}

	if _, err := lp.Publish(withBody(req, "first")); err != nil {
		t.Fatal(err)
	}
	ref, err := lp.Publish(withBody(req, "second"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(ref.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("re-run in the same period should overwrite, got %q", got)
	}
}

// TestLocalPublisher_NoTempFilesLeftBehind: the temp file must not
// survive a successful publish. A leftover ".name.tmp-*" in the period
// directory would show up in a docbuilder index or confuse an operator
// browsing the tree.
func TestLocalPublisher_NoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)
	ref, err := lp.Publish(Request{Name: "a--7d--team.md", Period: "2026-W42", Body: []byte("x"), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(ref.Ref))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestLocalPublisher_EmptyPeriodIsFlat: a request with no period (no
// caller does this today, but the field is optional) must not create a
// directory literally named "".
func TestLocalPublisher_EmptyPeriodIsFlat(t *testing.T) {
	dir := t.TempDir()
	lp := NewLocal(dir)
	ref, err := lp.Publish(Request{Name: "x--7d--team.md", Body: []byte("x"), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(ref.Ref) != dir {
		t.Errorf("empty period should write flat, got %s", ref.Ref)
	}
}

func withBody(r Request, b string) Request {
	r.Body = []byte(b)
	return r
}
