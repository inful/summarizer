package publisher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The prefix has no default and Validate does not require it, so both
// configurations reach here and the filter's narrowness depends on
// which one it is. The old comment claimed the same narrowness either
// way; these pin what each configuration actually claims.
//
// isWeekSlug tests the WHOLE name, so an empty prefix is not as open as
// it first looks: "other-tool/2026-W41" fails on length regardless. What
// an empty prefix does claim is a branch named exactly "YYYY-Wnn",
// which is what PeriodBranch produces for summarizer's own weeks.
func TestListPeriodBranches_PrefixDeterminesHowNarrowTheFilterIs(t *testing.T) {
	branches := []string{
		"main",
		"feature/someone-elses-work",
		"summarizer/2026-W42",
		"summarizer/2026-09",  // a monthly branch, not weekly
		"other-tool/2026-W41", // foreign, week-shaped
		"summarizer/2026-W4",  // malformed slug
		"2026-W40",            // week-shaped, unprefixed
	}
	for _, tc := range []struct {
		name   string
		prefix string
		want   []string
	}{
		{
			name:   "prefix set claims only its own",
			prefix: "summarizer",
			want:   []string{"summarizer/2026-W42"},
		},
		{
			// Correcting the audit that prompted this: an empty prefix
			// does NOT claim "other-tool/2026-W41". isWeekSlug checks
			// the whole name and that one is 19 characters.
			name:   "prefix empty claims only a bare week-shaped name",
			prefix: "",
			want:   []string{"2026-W40"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Next-Page", "")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"name":"main"},{"name":"feature/someone-elses-work"},` +
					`{"name":"summarizer/2026-W42"},{"name":"summarizer/2026-09"},` +
					`{"name":"other-tool/2026-W41"},{"name":"summarizer/2026-W4"},` +
					`{"name":"2026-W40"}]`))
			}))
			defer srv.Close()

			g := &GitLabPublisher{
				Host: srv.URL, Project: "g/p", Token: "tok", DefaultBranch: "main",
				SourceBranchPrefix: tc.prefix, HTTP: &http.Client{Timeout: 5 * time.Second},
			}
			got, err := g.ListPeriodBranches(context.Background())
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			found := make(map[string]bool, len(got))
			for _, b := range got {
				found[b] = true
			}
			for _, b := range tc.want {
				if !found[b] {
					t.Errorf("missing %q from %v", b, got)
				}
			}
			for _, b := range branches {
				if found[b] {
					continue
				}
				for _, w := range tc.want {
					if b == w {
						t.Errorf("unexpected branch %q in %v", b, got)
					}
				}
			}
		})
	}
}

// With the prefix set, a foreign week-shaped branch must never be read
// from — that is the property the filter exists for.
func TestListPeriodBranches_ForeignWeekShapedBranchIsNotClaimedWithAPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"other-tool/2026-W41"}]`))
	}))
	defer srv.Close()

	g := &GitLabPublisher{
		Host: srv.URL, Project: "g/p", Token: "tok", DefaultBranch: "main",
		SourceBranchPrefix: "summarizer", HTTP: &http.Client{Timeout: 5 * time.Second},
	}
	got, err := g.ListPeriodBranches(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("claimed %v — a foreign branch is not summarizer's weekly rollup", got)
	}
}
