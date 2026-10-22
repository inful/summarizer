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
	ListPeriodBranches(ctx context.Context) ([]string, error)
	ReadReportFile(ctx context.Context, branch, path string) (string, error)
	PeriodBranch(period string) string

	// DefaultRef is the ref to read from when the week's source branch
	// is gone. A weekly rollup is merged, not left on its branch: with
	// GitLab's "delete source branch on merge" on — a project setting,
	// or a box ticked in the merge dialog — the branch disappears after
	// the merge and the file survives only on the default branch.
	//
	// Without this fallback a fully-published month read as an empty
	// one and `summarizer monthly` wrote nothing, pointing the operator
	// at the target repo instead of at the setting.
	//
	// Named DefaultRef rather than DefaultBranch because
	// GitLabPublisher.DefaultBranch is a field.
	DefaultRef() string
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

	// Which weekly branches still exist in the target repo. A week's
	// file may be on the default branch instead — see MonthlyReader's
	// DefaultRef — so an absent branch is a reason to try main, not to
	// skip the week.
	branches, err := r.Reader.ListPeriodBranches(ctx)
	if err != nil {
		return res, fmt.Errorf("fetch branches: %w", err)
	}
	present := make(map[string]bool, len(branches))
	for _, b := range branches {
		present[b] = true
	}

	byWeek := make(map[prompt.Audience][]prompt.ProjectReport, 2)
	for _, week := range allWeeks {
		branch := r.Reader.PeriodBranch(week)
		// The source branch while it exists: it holds the newest content
		// while an MR is open. Once merged it may have been deleted, and
		// the default branch is the only copy — same path, same file,
		// because the branch was forked from it.
		ref := branch
		if !present[branch] {
			def := r.Reader.DefaultRef()
			if def == "" {
				res.Skipped = append(res.Skipped, week+" (no branch)")
				continue
			}
			ref = def
		}
		found := false
		for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
			body, err := r.Reader.ReadReportFile(ctx, ref, r.pathFor(branch, weeklyRollupName(aud)))
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

	// A week counts as read when EITHER audience published a rollup, but
	// the two documents are built from different weeks. runRollup
	// publishes no customer rollup for a week where every project was
	// team-only, so that week's branch carries a team file and no
	// customer file — the month-end customer document then covers one
	// week fewer than res.Weeks and nothing says so. Say it per
	// audience; the customer document is the one that is published
	// public: true.
	for _, aud := range []prompt.Audience{prompt.AudienceCustomer, prompt.AudienceTeam} {
		have := make(map[string]bool, len(byWeek[aud]))
		for _, w := range byWeek[aud] {
			have[w.Name] = true
		}
		var missing []string
		for _, week := range res.Weeks {
			if !have[week] {
				missing = append(missing, week)
			}
		}
		for _, week := range missing {
			res.Skipped = append(res.Skipped,
				fmt.Sprintf("%s (no %s rollup published that week)", week, aud))
		}
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
		out, err := r.Publisher.Publish(ctx, publisher.Request{
			Name:     monthlyRollupFilename(month, aud),
			Body:     []byte(resp.Text),
			Kind:     publisher.KindMonthly,
			Period:   monthlySlug(month),
			Audience: string(aud),
			Project:  "",
			Public:   aud == prompt.AudienceCustomer,
		})
		if err != nil {
			return res, fmt.Errorf("write monthly %s: %w", aud, err)
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

// weeklyRollupName is the one file this reader wants out of a week's
// directory. The name is fixed because the window is: summarizer reports
// on the current ISO week and nothing else, so there is no second window
// size to disambiguate, and no stale file that a reconfigured window
// could have left behind.
//
// The `rollup` kind segment is what separates this from the epic
// rollups, which share the prefix and the audience suffix. Naming the
// whole file rather than pattern-matching is deliberate: the epic
// rollups stay out of the monthly summary because they are already
// split per customer and folding them in would mix customers.
func weeklyRollupName(aud prompt.Audience) string {
	return fmt.Sprintf("combined--rollup--wk--%s.md", aud)
}

// pathFor rebuilds the repo-relative path the publisher writes to, so a
// month's read lands on the same files a week's write created.
//
// The branch name carries the period as its last segment; the rest of
// the assembly is publisher.TargetPath's, shared with the writer rather
// than copied — the prefix half of this exact string had already shipped
// as a bug, where "reports/" wrote to reports/… and read from reports//…
// so every week 404'd and the month reported itself empty.
func (r *MonthlyRunner) pathFor(branch, name string) string {
	period := branch
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		period = branch[i+1:]
	}
	return publisher.TargetPath(r.ReportPrefix, period, name)
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

// The monthly reader is implemented by exactly one type, and saying so
// at compile time is cheaper than discovering it at runtime.
//
// *publisher.Local deliberately does NOT implement it — it lacks
// ListPeriodBranches and PeriodBranch, so it cannot satisfy this
// interface at all. It used to carry DefaultRef and ReadReportFile
// stubs anyway, on the stated theory that the interface "deliberately
// does not include" ReadReportFile. It does include it (three lines
// above), and *Local could never be behind an interface value here
// regardless, so both stubs were unreachable: cmd/summarizer rejects a
// local-disk publisher with a type assertion before a runner is built.
// Deleting them is what makes this assertion the whole story.
var _ MonthlyReader = (*publisher.GitLabPublisher)(nil)
