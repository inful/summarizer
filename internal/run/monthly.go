package run

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/inful/summarizer/internal/llm"
	"github.com/inful/summarizer/internal/prompt"
	"github.com/inful/summarizer/internal/publisher"
	"github.com/inful/summarizer/internal/safety"
)

// MonthlyReader is what a monthly run needs from a publisher: the
// ability to list the weekly branches and read one file off one. Not
// part of publisher.Publisher, because a local-disk publisher has no
// remote repo to read and forcing it to stub these would hide that.
type MonthlyReader interface {
	ListPeriodBranches() ([]string, error)
	ReadReportFile(branch, path string) (string, error)
	PeriodBranch(period string) string
}

// MonthlyRunner rolls up a whole calendar month's weekly rollups into
// one customer-facing and one team-facing file.
//
// It is deliberately separate from Pipeline.Run: a monthly run reads
// last month's *output* rather than re-fetching GitLab activity, so it
// needs none of the fetch/classify/publish machinery. Re-running the
// pipeline for September would mean a second full pass of per-project
// LLM calls to produce text that already exists as that month's weekly
// rollups.
type MonthlyRunner struct {
	Publisher publisher.Publisher
	Reader    MonthlyReader // must be the same object as Publisher
	LLM       llm.Provider
	Guard     safety.Guard // injection guard; NoopGuard when disabled
	Model     string
	DryRun    bool

	// ReportPrefix is the publisher's path prefix, e.g. "reports". The
	// weekly files live at <prefix>/<week>/<name>, so the reader has to
	// reconstruct the same path the writer used.
	ReportPrefix string
}

// MonthlyResult is the outcome of one monthly rollup.
type MonthlyResult struct {
	Month    string   // "2026-09"
	Weeks    []string // week slugs found, sorted; those actually read
	Files    []string // published refs
	Skipped  []string // weeks with no matching rollup file
	Customer bool     // a customer-audience file was written
	Team     bool     // a team-audience file was written
}

// Run rolls up one month. month is "YYYY-MM"; pass "" for the
// previous calendar month.
//
// One LLM call per audience, fed the weekly rollups of that audience
// only — the customer prompt never sees the team text.
func (r *MonthlyRunner) Run(ctx context.Context, month string) (MonthlyResult, error) {
	res := MonthlyResult{Month: month}
	if month == "" {
		month = previousMonth(time.Now().UTC())
		res.Month = month
	}

	allWeeks, err := weeksInMonth(month)
	if err != nil {
		return res, err
	}

	if r.Reader == nil {
		return res, fmt.Errorf("monthly rollups need a GitLab target repo (publish.enabled); this build has no reader")
	}

	// Which weekly branches actually exist in the target repo. A week
	// with no branch means the cron didn't run (or its MR was never
	// pushed); skip it rather than failing the whole month.
	branches, err := r.Reader.ListPeriodBranches()
	if err != nil {
		return res, fmt.Errorf("list period branches: %w", err)
	}
	present := make(map[string]bool, len(branches))
	for _, b := range branches {
		present[b] = true
	}

	byWeek := make(map[prompt.Audience][]prompt.ProjectReport, 2)
	for _, week := range allWeeks {
		branch := r.Reader.PeriodBranch(week)
		if !present[branch] {
			res.Skipped = append(res.Skipped, week+" (no branch)")
			continue
		}
		found := false
		for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
			body, err := r.readWeeklyRollup(branch, aud)
			if err != nil {
				return res, err
			}
			if body == "" {
				continue
			}
			byWeek[aud] = append(byWeek[aud], prompt.ProjectReport{Name: week, Body: body})
			found = true
		}
		if found {
			res.Weeks = append(res.Weeks, week)
		} else {
			res.Skipped = append(res.Skipped, week+" (no rollup file)")
		}
	}

	if len(res.Weeks) == 0 {
		return res, fmt.Errorf("no weekly rollups found for %s in the target repo", month)
	}

	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		weeks := byWeek[aud]
		if len(weeks) == 0 {
			continue
		}
		system, user, err := prompt.RenderMonthlyRollup(aud, month, weeks)
		if err != nil {
			return res, fmt.Errorf("render monthly rollup %s: %w", aud, err)
		}
		if r.DryRun {
			fmt.Printf("--- monthly %s %s (%d weeks, %d bytes prompt) ---\n%s\n",
				month, aud, len(weeks), len(user), user)
			continue
		}
		if err := checkPrompt(ctx, r.Guard, "monthly-rollup/"+month+"/"+string(aud), system+"\n\n"+user); err != nil {
			return res, err
		}
		resp, err := r.LLM.Complete(ctx, llm.Request{
			System: system, User: user, Model: r.Model, MaxTokens: 2048,
		})
		if err != nil {
			return res, fmt.Errorf("llm monthly %s: %w", aud, err)
		}
		out, err := r.Publisher.Publish(publisher.Request{
			Name:    monthlyRollupFilename(month, aud),
			Body:    []byte(resp.Text),
			Force:   true,
			Kind:    publisher.KindRollup,
			Period:  monthlySlug(month),
			Title:   "monthly rollup " + string(aud),
			Project: "",
			Public:  aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return res, fmt.Errorf("publish monthly %s: %w", aud, err)
		}
		res.Files = append(res.Files, out.Ref)
		if aud == prompt.AudienceCustomer {
			res.Customer = true
		} else {
			res.Team = true
		}
	}
	return res, nil
}

// readWeeklyRollup fetches one week's cross-project rollup for one
// audience off its branch. Returns "" when the branch carries none.
//
// Only the plain `combined--<N>d--<audience>.md` file is read. The
// per-customer epic rollups (`combined--epics-<slug>--<N>d--customer.md`)
// are deliberately excluded: they are already split per customer, and
// folding them into one monthly blob would mix customers together —
// the opposite of what that split was built to prevent.
func (r *MonthlyRunner) readWeeklyRollup(branch string, aud prompt.Audience) (string, error) {
	for _, days := range []int{1, 7, 14, 30} {
		name := fmt.Sprintf("combined--%dd--%s.md", days, aud)
		body, err := r.Reader.ReadReportFile(branch, r.pathFor(branch, name))
		if err != nil {
			return "", fmt.Errorf("read %s from %s: %w", name, branch, err)
		}
		if body != "" {
			return body, nil
		}
	}
	return "", nil
}

// pathFor rebuilds the repo-relative path the publisher writes to.
// It mirrors GitLabPublisher.targetPath; kept as a method so the
// monthly runner works against either publisher.
func (r *MonthlyRunner) pathFor(branch, name string) string {
	// Strip the source-branch prefix to get back the period slug.
	period := branch
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		period = branch[i+1:]
	}
	if r.ReportPrefix == "" {
		return period + "/" + name
	}
	return r.ReportPrefix + "/" + period + "/" + name
}

// weeksInMonth returns the ISO week slugs whose Monday falls inside
// the given YYYY-MM month, in ascending order.
//
// Iterating Mondays rather than days matters: a week belongs to the
// month containing its Monday, so the week of Sep 28 2026 (which runs
// into October) is a September week and must not be double-counted in
// October. Days 1..7 of a month always contain at least one Monday, so
// the loop always yields at least four weeks.
func weeksInMonth(month string) ([]string, error) {
	year, mon, err := parseMonth(month)
	if err != nil {
		return nil, err
	}
	first := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, time.UTC)
	// Advance to the first Monday ON OR AFTER the 1st. (Not the Monday
	// of the week containing the 1st — that Monday is usually in the
	// previous month, and including it would count one week in two
	// consecutive monthly rollups.)
	start := first.AddDate(0, 0, (8-int(first.Weekday()))%7)
	nextMonth := time.Date(year, time.Month(mon)+1, 1, 0, 0, 0, 0, time.UTC)

	out := make([]string, 0, 5)
	for d := start; d.Before(nextMonth); d = d.AddDate(0, 0, 7) {
		out = append(out, weekSlugFor(d))
	}
	return out, nil
}

// parseMonth validates a "YYYY-MM" string and returns the year and
// month number. time.Parse enforces exactly the rules we want — exact
// "2006-01" layout, digits only, month 1-12 — and produces a clearer
// error than a hand-rolled length-and-digits check.
func parseMonth(s string) (year, month int, err error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return 0, 0, fmt.Errorf("month %q: want YYYY-MM", s)
	}
	return t.Year(), int(t.Month()), nil
}

// previousMonth returns the YYYY-MM before t's month, rolling back
// across the year boundary. Used as the default for --month so a cron
// on the 1st rolls up the month that just closed.
func previousMonth(t time.Time) string {
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, -1, 0).Format("2006-01")
}

// monthlySlug is the period bucket for a monthly rollup: the month
// itself, e.g. "2026-09". Distinct from the weekly "2026-W36" slugs so
// the monthly MR never collides with a week's.
func monthlySlug(month string) string { return month }

// monthlyRollupFilename names the published file. "monthly" replaces
// the "combined" prefix the weekly rollups use, so an operator
// browsing the target repo can tell the two apart without rendering.
func monthlyRollupFilename(month string, aud prompt.Audience) string {
	return fmt.Sprintf("monthly--%s--%s.md", month, aud)
}
