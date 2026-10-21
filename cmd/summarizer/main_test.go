package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/config"
	"github.com/inful/summarizer/internal/run"
	"gopkg.in/yaml.v3"
)

// TestUnionSlices_DedupesAndKeepsOrder: --internal-only-project and the
// YAML block are unioned; the first occurrence wins and order is stable
// so the run manifest reads predictably.
func TestUnionSlices_DedupesAndKeepsOrder(t *testing.T) {
	got := unionSlices([]string{"a", "b"}, []string{"b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(unionSlices(nil, nil)) != 0 {
		t.Error("two nil inputs should yield an empty slice, not nil-ish garbage")
	}
}

// TestClassify_TypedSentinelsWin: the sentinel we can match reliably
// must NOT depend on the message text. ErrInjection carries the word
// "write"-adjacent vocabulary in some wrappings, but a wrapped
// error's outer text is what classify sees.
func TestClassify_TypedSentinelsWin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"injection", fmt.Errorf("project g/p: %w", run.ErrInjection), exitInjection},
		{"gitlab", errors.New("fetch mrs: 500 boom"), exitGitLab},
		{"llm", errors.New("llm customer: connection reset"), exitLLM},
		{"write", errors.New("write team: disk full"), exitWrite},
		{"config", errors.New("config: bad host"), exitConfig},
		{"render", errors.New("render customer: template broke"), exitLLM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Errorf("classify(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestClassify_UnclassifiedIsDocumented: an unrecognised failure must
// return the documented exitUnknown code, not a bare literal, so the
// README table stays honest.
func TestClassify_UnclassifiedIsDocumented(t *testing.T) {
	got := classify(errors.New("something nobody predicted"))
	if got != exitUnknown {
		t.Errorf("classify = %d, want exitUnknown (%d)", got, exitUnknown)
	}
	if exitUnknown != 1 {
		t.Errorf("exitUnknown = %d; README documents 1", exitUnknown)
	}
}

// TestPrintSummary_CountsFilesActuallyWritten: the summary said
// "2 * len(projects)" plus rollups, which over-counted every team-only
// project (one file, not two) and ignored the epic rollups entirely.
// An operator reconciling the target repo against this line could not.
func TestPrintSummary_CountsFilesActuallyWritten(t *testing.T) {
	res := run.Result{
		Projects: []run.ProjectResult{
			{Project: "a", EventsFetched: 10, EventsUsed: 10, FilesWritten: []string{"a.md"}, CustomerOn: true},
			{Project: "b", EventsFetched: 5, EventsUsed: 5, FilesWritten: []string{"b.md"}, TeamOnly: true},
			{Project: "c", EventsFetched: 7, EventsUsed: 7, FilesWritten: []string{"c.md"}}, // 2 files
		},
		RollupFiles:     []string{"r1", "r2"},
		EpicRollupFiles: []string{"e1"},
	}
	out := captureStderr(t, func() { printSummary(res, nil, false, time.Now()) })
	if !strings.Contains(out, "6 file(s) written") {
		t.Errorf("want 6 files (3 per-project + 2 rollups + 1 epic rollup), got:\n%s", out)
	}
	if !strings.Contains(out, "e1 (epics rollup)") {
		t.Errorf("epic rollup files should be listed alongside the others:\n%s", out)
	}
	if !strings.Contains(out, "22 event(s)") {
		t.Errorf("event totals wrong:\n%s", out)
	}
}

// TestPrintSummary_NoFileClaimOnDryRun: a dry run writes nothing, so it
// must not claim it did.
func TestPrintSummary_NoFileClaimOnDryRun(t *testing.T) {
	res := run.Result{Projects: []run.ProjectResult{{Project: "a", FilesWritten: []string{"a.md"}}}}
	out := captureStderr(t, func() { printSummary(res, nil, true, time.Now()) })
	if strings.Contains(out, "file(s) written") {
		t.Errorf("dry run must not claim writes:\n%s", out)
	}
}

// TestPrintSummary_SurfacesTheError: the run's stderr line is the only
// place the operator sees what failed.
func TestPrintSummary_SurfacesTheError(t *testing.T) {
	out := captureStderr(t, func() {
		printSummary(run.Result{}, errors.New("probe g/p: 403 forbidden"), false, time.Now())
	})
	if !strings.Contains(out, "403 forbidden") {
		t.Errorf("error not surfaced:\n%s", out)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// what was written. printSummary writes straight to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

// TestConfigExampleIsLoadable: examples/summarizer.yaml is the file the
// README tells operators to copy. It must parse into the struct, and
// every key it uses must be one the loader binds.
func TestConfigExampleIsLoadable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "summarizer.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	// Unknown-field strictness is the whole point: yaml.Unmarshal is
	// permissive by default, which is how a key that binds to nothing
	// ships unnoticed.
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	cfg := &config.Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		t.Errorf("example config has keys the loader does not bind: %v", err)
	}
	if cfg.GitLabHost == "" {
		t.Error("example config sets gitlab_host but the loader did not bind it")
	}
}
