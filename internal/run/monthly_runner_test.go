package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/publisher"
)

// monthlyRepo is a fake GitLab target repo holding published weekly
// rollups. Branches are the summarizer's own weekly ones; files maps
// "<branch>|<path>" to a body.
type monthlyRepo struct {
	branches []string
	files    map[string]string
}

func (r *monthlyRepo) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/repository/branches"):
			out := make([]map[string]any, 0, len(r.branches))
			for _, b := range r.branches {
				out = append(out, map[string]any{"name": b})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.Contains(p, "/repository/tree"):
			// The monthly rollup lists a week's directory so it can
			// tell one window size from two. Derive the entries from
			// the same files map the file reads use.
			dir, ref := req.URL.Query().Get("path"), req.URL.Query().Get("ref")
			seen := map[string]bool{}
			out := make([]map[string]any, 0, 4)
			for key := range r.files {
				branch, file, ok := strings.Cut(key, "|")
				if !ok || branch != ref || !strings.HasPrefix(file, dir+"/") {
					continue
				}
				name := file[len(dir)+1:]
				if strings.Contains(name, "/") || seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, map[string]any{"name": name, "type": "blob"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.Contains(p, "/repository/files/"):
			// Writes are POST (create) / PUT (update). The monthly
			// run only needs them to succeed; it never reads back what
			// it wrote.
			if req.Method != http.MethodGet {
				w.WriteHeader(http.StatusCreated)
				return
			}
			branch := req.URL.Query().Get("ref")
			idx := strings.Index(p, "/repository/files/")
			name := p[idx+len("/repository/files/"):]
			// Paths arrive percent-encoded (%2F for the separators we
			// deliberately escape); decode just enough for the map key.
			var sb strings.Builder
			for i := 0; i < len(name); i++ {
				if name[i] == '%' && i+2 < len(name) {
					if v, ok := strconvAtoiHex(name[i+1], name[i+2]); ok {
						sb.WriteByte(v)
						i += 2
						continue
					}
				}
				sb.WriteByte(name[i])
			}
			body, ok := r.files[branch+"|"+sb.String()]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"encoding": "text", "content": body, "last_commit_id": "sha",
			})
		case strings.Contains(p, "/repository/branches/"):
			// branch create (not exercised by a read-only monthly run)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": req.URL.Path})
		case strings.Contains(p, "/merge_requests"):
			w.Header().Set("Content-Type", "application/json")
			if req.Method == http.MethodGet {
				// No open MR for this branch yet; the publisher
				// creates one.
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 1, "web_url": "https://gitlab.example/mr/1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func strconvAtoiHex(a, b byte) (byte, bool) {
	hi, ok1 := hexVal(a)
	lo, ok2 := hexVal(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

type monthlyCapturedLLM struct{ prompts []string }

func (m *monthlyCapturedLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	m.prompts = append(m.prompts, req.User)
	return llm.Response{Text: "monthly summary body"}, nil
}

func newMonthlyRunner(t *testing.T, repo *monthlyRepo) (*MonthlyRunner, *monthlyCapturedLLM) {
	t.Helper()
	srv := httptest.NewServer(repo.handler())
	t.Cleanup(srv.Close)
	glPub := publisher.NewGitLab(srv.URL, "glpat-x", "sfb/reports", "reports", "main", "summarizer")
	llmStub := &monthlyCapturedLLM{}
	return &MonthlyRunner{
		Publisher:    glPub,
		Reader:       glPub,
		LLM:          llmStub,
		Model:        "test-model",
		ReportPrefix: "reports",
	}, llmStub
}

// weeklyFiles builds the published-file map for four weeks of
// September 2026, each carrying a customer and a team rollup whose
// body names its week so the monthly prompt can be checked for them.
func weeklyFiles(weeks ...string) map[string]string {
	files := map[string]string{}
	for _, w := range weeks {
		for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
			path := "reports/" + w + "/combined--rollup--wk--" + string(aud) + ".md"
			files["summarizer/"+w+"|"+path] = "---\nuid: x\n---\n\nweekly body for " + w + " " + string(aud)
		}
	}
	return files
}

// TestMonthlyRunner_RefusesAmbiguousWindow: a weekly branch keeps every
// file it ever received, and the window size is part of the filename.
// Changing --since mid-week therefore leaves both a 1d and a 7d rollup
// side by side. The old code tried 1d, 7d, 14d, 30d in order and took
// the first hit, feeding a one-day rollup into the month summary
// without a word. There is no modification time in the Files API, so
// the honest answer is to name the conflict and skip the week.
func TestMonthlyRunner_RefusesAmbiguousWindow(t *testing.T) {
	weeks, err := weeksInMonth("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	bad := weeks[0]
	files := weeklyFiles(weeks...)
	// A stale run from earlier in the same week, at a different window.
	files["summarizer/"+bad+"|reports/"+bad+"/combined--rollup--30d--customer.md"] =
		"---\nuid: y\n---\n\nstale thirty-day body"
	files["summarizer/"+bad+"|reports/"+bad+"/combined--rollup--30d--team.md"] =
		"---\nuid: y\n---\n\nstale thirty-day body"

	repo := &monthlyRepo{branches: append([]string{"main"}, prefixes(weeks...)...), files: files}
	r, _ := newMonthlyRunner(t, repo)

	res, err := r.Run(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Weeks) != len(weeks)-1 {
		t.Errorf("the ambiguous week should be skipped; got %d of %d: %v",
			len(res.Weeks), len(weeks), res.Weeks)
	}
	var skipped bool
	for _, s := range res.Skipped {
		if strings.HasPrefix(s, bad) {
			skipped = true
			if !strings.Contains(s, "combined--rollup--30d") {
				t.Errorf("the skip reason should name the conflicting files, got %q", s)
			}
		}
	}
	if !skipped {
		t.Errorf("week %s should be listed as skipped, got %v", bad, res.Skipped)
	}
	for _, w := range res.Weeks {
		if w == bad {
			t.Errorf("week %s was ambiguous and must not be summarised", bad)
		}
	}
}

// TestMonthlyRunner_RollsUpEveryWeekOfTheMonth: the headline case. All
// four September weeks are read, both audiences are written, and each
// prompt carries every week's text and nothing from another month.
func TestMonthlyRunner_RollsUpEveryWeekOfTheMonth(t *testing.T) {
	weeks, err := weeksInMonth("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	repo := &monthlyRepo{
		branches: append([]string{"main"}, prefixes(weeks...)...),
		files:    weeklyFiles(weeks...),
	}
	r, llmStub := newMonthlyRunner(t, repo)

	res, err := r.Run(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Weeks) != len(weeks) {
		t.Fatalf("read %d weeks, want %d: %v", len(res.Weeks), len(weeks), res.Weeks)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("nothing should be skipped, got %v", res.Skipped)
	}
	if !res.Customer || !res.Team {
		t.Errorf("expected both audiences, got customer=%v team=%v", res.Customer, res.Team)
	}
	if len(res.Files) != 2 {
		t.Errorf("expected 2 published files, got %v", res.Files)
	}
	if len(llmStub.prompts) != 2 {
		t.Fatalf("expected one LLM call per audience, got %d", len(llmStub.prompts))
	}
	// Each prompt must carry every week's body for its own audience,
	// and must not leak the other audience's text.
	var sawCustomer, sawTeam bool
	for i, p := range llmStub.prompts {
		for _, w := range weeks {
			if !strings.Contains(p, "weekly body for "+w+" ") {
				t.Errorf("prompt %d missing week %s", i, w)
			}
		}
		if strings.Contains(p, "customer") {
			sawCustomer = true
		}
		if strings.Contains(p, "team") {
			sawTeam = true
		}
	}
	if !sawCustomer || !sawTeam {
		t.Errorf("expected both audiences across the two prompts (customer=%v team=%v)", sawCustomer, sawTeam)
	}
}

// TestMonthlyRunner_SkipsWeeksWithNoBranch: a week whose cron didn't
// run leaves no branch. It must be skipped and reported, not treated
// as a hard failure — a five-week month with one missed run should
// still produce a rollup.
func TestMonthlyRunner_SkipsWeeksWithNoBranch(t *testing.T) {
	weeks, _ := weeksInMonth("2026-09")
	present := weeks[:2]
	repo := &monthlyRepo{
		branches: prefixes(present...),
		files:    weeklyFiles(present...),
	}
	r, _ := newMonthlyRunner(t, repo)

	res, err := r.Run(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Weeks) != 2 {
		t.Errorf("read %v, want the 2 published weeks", res.Weeks)
	}
	if len(res.Skipped) != 2 {
		t.Errorf("expected 2 skipped weeks, got %v", res.Skipped)
	}
	for _, s := range res.Skipped {
		if !strings.Contains(s, "no branch") {
			t.Errorf("skip reason should say why: %q", s)
		}
	}
}

// TestMonthlyRunner_NoWeeksIsAnError: rolling up a month where nothing
// was ever published must fail loudly rather than publishing an empty
// or hallucinated month-end summary.
func TestMonthlyRunner_NoWeeksIsAnError(t *testing.T) {
	r, llmStub := newMonthlyRunner(t, &monthlyRepo{branches: []string{"main"}})
	if _, err := r.Run(context.Background(), "2026-09"); err == nil {
		t.Error("expected an error when no weekly rollups exist")
	}
	if len(llmStub.prompts) != 0 {
		t.Error("must not call the LLM when there is nothing to roll up")
	}
}

// TestMonthlyRunner_BadMonthRejected: a typo in --month fails before
// any network call.
func TestMonthlyRunner_BadMonthRejected(t *testing.T) {
	r, _ := newMonthlyRunner(t, &monthlyRepo{})
	if _, err := r.Run(context.Background(), "september"); err == nil {
		t.Error("expected an error for a non-YYYY-MM month")
	}
}

// TestMonthlyRunner_DryRunMakesNoLLLCall: --dry-run assembles the
// prompt and prints it; it must not spend an LLM call or publish.
func TestMonthlyRunner_DryRunMakesNoLLLCall(t *testing.T) {
	weeks, _ := weeksInMonth("2026-09")
	repo := &monthlyRepo{branches: prefixes(weeks...), files: weeklyFiles(weeks...)}
	r, llmStub := newMonthlyRunner(t, repo)
	r.DryRun = true

	res, err := r.Run(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(llmStub.prompts) != 0 {
		t.Error("dry-run must not call the LLM")
	}
	if len(res.Files) != 0 {
		t.Errorf("dry-run must not publish, got %v", res.Files)
	}
}

// TestMonthlyRunner_DefaultMonthIsPrevious: with no --month the runner
// rolls up the previous calendar month.
func TestMonthlyRunner_DefaultMonthIsPrevious(t *testing.T) {
	r, _ := newMonthlyRunner(t, &monthlyRepo{branches: []string{"main"}})
	// No branches at all → error, but the error must name the month it
	// chose, which pins the default.
	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("expected an error with no published weeks")
	}
	want := previousMonth(time.Now().UTC())
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error should name the default month %q, got: %v", want, err)
	}
}

func prefixes(weeks ...string) []string {
	out := make([]string, 0, len(weeks))
	for _, w := range weeks {
		out = append(out, "summarizer/"+w)
	}
	return out
}

// TestMonthlyRunner_IgnoresEpicRollupsOnTheSameWeek: the epic rollups
// share the combined rollup's prefix and its audience suffix, with more
// `--` between them. Matched on prefix and suffix — which is what the
// ambiguity check did when it was written — every week carrying epics
// looked like it had two window sizes, so the whole week was skipped and
// the monthly summary silently lost it. The skip reason named the epic
// files as if they were competing windows, which is the confusion that
// should have made the collision obvious the first time.
func TestMonthlyRunner_IgnoresEpicRollupsOnTheSameWeek(t *testing.T) {
	weeks, err := weeksInMonth("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	files := weeklyFiles(weeks...)
	for _, w := range weeks {
		for _, name := range []string{
			"combined--epics-rollup--wk--customer.md",
			"combined--epics-rollup--wk--team.md",
			"combined--epics-acme--wk--customer.md",
			"combined--epics-acme--wk--team.md",
		} {
			files["summarizer/"+w+"|reports/"+w+"/"+name] =
				"---\nuid: z\n---\n\nepic rollup body"
		}
	}

	repo := &monthlyRepo{branches: append([]string{"main"}, prefixes(weeks...)...), files: files}
	r, llmStub := newMonthlyRunner(t, repo)

	res, err := r.Run(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Weeks) != len(weeks) {
		t.Errorf("epic rollups must not count as competing windows; read %d of %d weeks: skipped %v",
			len(res.Weeks), len(weeks), res.Skipped)
	}
	// And the epic text must not reach the prompt.
	for _, p := range llmStub.prompts {
		if strings.Contains(p, "epic rollup body") {
			t.Error("an epic rollup body leaked into the monthly prompt")
		}
	}
}
