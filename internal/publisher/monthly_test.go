package publisher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// monthlyServer stands in for the GitLab target repo. It serves the
// three endpoints a monthly rollup reads:
//
//	GET /repository/branches                          → list of branch names
//	GET /repository/files/:path?ref=                  → one file's content
//
// files maps "<branch><path>" to the raw body.
type monthlyServer struct {
	branches []string
	files    map[string]string // "branchpath" → content
	gets     int
}

func (m *monthlyServer) key(branch, path string) string { return branch + "|" + path }

func (m *monthlyServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.gets++
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/repository/branches"):
			out := make([]map[string]any, 0, len(m.branches))
			for _, b := range m.branches {
				out = append(out, map[string]any{"name": b})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)

		case strings.Contains(p, "/repository/files/"):
			branch := r.URL.Query().Get("ref")
			// Path arrives percent-encoded; the project segment is
			// already escaped so only the file path needs decoding.
			idx := strings.Index(p, "/repository/files/")
			raw := p[idx+len("/repository/files/"):]
			decoded, err := decodePath(raw)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body, ok := m.files[m.key(branch, decoded)]
			if !ok {
				http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fileResponse{
				Encoding: "text", Content: body, LastCommitID: "sha-" + decoded,
			})

		case strings.Contains(p, "/repository/archive"):
			// Not used by the read path; present so an accidental call
			// fails loudly rather than silently matching another case.
			http.Error(w, "unexpected", http.StatusInternalServerError)

		default:
			http.Error(w, "unhandled "+p, http.StatusNotFound)
		}
	})
}

func decodePath(s string) (string, error) {
	out := []byte(s)
	for i := 0; i < len(out); i++ {
		if out[i] != '%' || i+2 >= len(out) {
			continue
		}
		hi, ok1 := hexVal(out[i+1])
		lo, ok2 := hexVal(out[i+2])
		if ok1 && ok2 {
			out[i] = hi<<4 | lo
			copy(out[i+1:], out[i+3:])
			out = out[:len(out)-2]
		}
	}
	return string(out), nil
}

func hexVal(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

func newMonthlyPublisher(t *testing.T, ms *monthlyServer) *GitLabPublisher {
	t.Helper()
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)
	return NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")
}

// TestListPeriodBranches_OnlyWeeklyBranches: the monthly rollup walks
// the weeks of a month, so the branch listing must return only the
// publisher's own period branches and not unrelated branches (other
// tools, manual work) or the target's default branch.
func TestListPeriodBranches_OnlyWeeklyBranches(t *testing.T) {
	ms := &monthlyServer{branches: []string{
		"summarizer/2026-W36", "summarizer/2026-W37", "summarizer/2026-W38",
		"main", "somebody/feature-work", "summarizer/not-a-week",
	}}
	p := newMonthlyPublisher(t, ms)

	got, err := p.ListPeriodBranches(context.Background())
	if err != nil {
		t.Fatalf("ListPeriodBranches: %v", err)
	}
	want := []string{"summarizer/2026-W36", "summarizer/2026-W37", "summarizer/2026-W38"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("branch %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestListPeriodBranches_EmptyRepo: a repo with no published weeks yet
// returns an empty slice, not an error.
func TestListPeriodBranches_EmptyRepo(t *testing.T) {
	p := newMonthlyPublisher(t, &monthlyServer{})
	got, err := p.ListPeriodBranches(context.Background())
	if err != nil {
		t.Fatalf("ListPeriodBranches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestReadReportFile: the monthly rollup reads last month's weekly
// rollups back as data. The body must come back verbatim — frontmatter
// included or not is the caller's business, but a mangled body would
// feed the LLM garbage.
func TestReadReportFile(t *testing.T) {
	body := "---\nuid: abc-123\npublic: true\n---\n\n# Week 36\n\nWe shipped the thing.\n"
	ms := &monthlyServer{
		branches: []string{"summarizer/2026-W36"},
		files: map[string]string{
			"summarizer/2026-W36|reports/2026-W36/combined--7d--customer.md": body,
		},
	}
	p := newMonthlyPublisher(t, ms)

	got, err := p.ReadReportFile(context.Background(), "summarizer/2026-W36", "reports/2026-W36/combined--7d--customer.md")
	if err != nil {
		t.Fatalf("ReadReportFile: %v", err)
	}
	if got != body {
		t.Errorf("body round-trip mismatch:\ngot:  %q\nwant: %q", got, body)
	}
}

// TestReadReportFile_Missing: a week whose rollup was never published
// (or was deleted) reports absence rather than erroring, so one missing
// week doesn't kill the whole monthly rollup.
func TestReadReportFile_Missing(t *testing.T) {
	p := newMonthlyPublisher(t, &monthlyServer{})
	if _, err := p.ReadReportFile(context.Background(), "summarizer/2026-W36", "reports/2026-W36/combined--7d--customer.md"); err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
}

// TestReadReportFile_Base64: GitLab returns encoding="base64" for some
// payloads. Decoding must handle it or the monthly prompt receives
// base64 gibberish.
func TestReadReportFile_Base64(t *testing.T) {
	plain := "# Week 36\n\nreal markdown\n"
	enc := base64.StdEncoding.EncodeToString([]byte(plain))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repository/files/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fileResponse{Encoding: "base64", Content: enc})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "summarizer/2026-W36"}})
	}))
	defer srv.Close()
	p := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer")

	got, err := p.ReadReportFile(context.Background(), "summarizer/2026-W36", "reports/2026-W36/combined--7d--customer.md")
	if err != nil {
		t.Fatalf("ReadReportFile: %v", err)
	}
	if got != plain {
		t.Errorf("base64 body not decoded: got %q, want %q", got, plain)
	}
}

// TestLocalPublisher_ReadReportFileUnsupported: the local publisher
// has no GitLab repo to read from. The monthly command must be able to
// say so rather than silently producing an empty rollup.
func TestLocalPublisher_ReadReportFileUnsupported(t *testing.T) {
	lp := NewLocal(t.TempDir())
	if _, err := lp.ReadReportFile(context.Background(), "x", "y"); err == nil {
		t.Error("local publisher should reject ReadReportFile")
	}
}

// TestListPeriodBranches_PagesUntilGitLabStops: a reports repo
// accrues one branch per week and never loses them, so a single
// per_page=100 stopped returning recent weeks in the second year — and
// `summarizer monthly` reported those weeks as "no rollup file" while
// still exiting zero. The monthly branches the code discards on arrival
// cost 12 slots a year of that budget, because the server had already
// spent them.
func TestListPeriodBranches_PagesUntilGitLabStops(t *testing.T) {
	const perPage = 100
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		w.Header().Set("Content-Type", "application/json")
		// Three pages then done, so the loop must stop at three rather
		// than run to maxBranchPages.
		if page < 3 {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		} else {
			w.Header().Del("X-Next-Page")
		}
		var list []map[string]string
		for i := 0; i < perPage; i++ {
			// A distinct name per row, so a short read cannot hide
			// behind deduplication.
			// A different year per page, so all 300 rows are distinct
			// week slugs and none is filtered — the count below is the
			// count the server sent, not a deduplicated one.
			list = append(list, map[string]string{
				"name": fmt.Sprintf("summarizer/%d-W%02d", 2000+page, i%52+1),
			})
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer srv.Close()

	got, err := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer").
		ListPeriodBranches(context.Background())
	if err != nil {
		t.Fatalf("ListPeriodBranches: %v", err)
	}
	if requests < 3 {
		t.Errorf("made %d requests, want at least 3 to see past the first page", requests)
	}
	// 3 pages × 100 rows.
	if len(got) != 300 {
		t.Errorf("collected %d branches, want 156 — the listing stopped at one page", len(got))
	}
}

// TestListPeriodBranches_StopsOnTheLastPage: the loop must end when
// GitLab stops offering more, not after maxBranchPages.
func TestListPeriodBranches_StopsOnTheLastPage(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("X-Next-Page") // last page
		_, _ = w.Write([]byte(`[{"name":"summarizer/2026-W43"}]`))
	}))
	defer srv.Close()

	got, err := NewGitLab(srv.URL, "glpat-x", "group/reports", "reports", "main", "summarizer").
		ListPeriodBranches(context.Background())
	if err != nil {
		t.Fatalf("ListPeriodBranches: %v", err)
	}
	if requests != 1 {
		t.Errorf("made %d requests, want 1", requests)
	}
	if len(got) != 1 {
		t.Errorf("got %v, want one week branch", got)
	}
}
