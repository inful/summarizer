package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Kubernetes deployment in docs/kubernetes.md is a manifest for a
// binary whose behaviour it describes: the schedule it fires on, the
// deadline it gives the run, the image it pulls and the exit codes it
// can return. Nothing checks any of it, and a manifest is the easiest
// file in a repo to forget — it keeps looking right while the code
// moves underneath it.
//
// The invariants below are the ones that would actually bite:
//
//   - the weekly schedule must be the one the runbook documents for
//     cron, so the two deployment paths cannot drift apart;
//   - the pod's deadline must be LONGER than the binary's own, or
//     Kubernetes kills the run before it can print its summary and
//     say which projects it had already published;
//   - the image must be a tag the release actually pushes;
//   - every exit code the document names must be one the binary
//     returns.

// weeklyManifest returns the part of the document from the weekly
// CronJob onwards, so the monthly one is not mistaken for it.
func weeklyManifest(t *testing.T) string {
	t.Helper()
	doc := readDoc(t, "..", "..", "docs", "kubernetes.md")
	i := strings.Index(doc, "name: summarizer-weekly")
	if i < 0 {
		t.Fatal("docs/kubernetes.md has no summarizer-weekly CronJob")
	}
	return doc[i:]
}

// TestKubernetesDoc_WeeklyScheduleMatchesTheRunbookCron: the runbook
// documents "0 23 * * 0" as the cadence, and says why — the window is
// the ISO week, so a Sunday run covers 167 hours and a Monday run
// covers three. The Kubernetes manifest schedules the same run; if the
// two ever disagree, one of them is reporting on the wrong week.
func TestKubernetesDoc_WeeklyScheduleMatchesTheRunbookCron(t *testing.T) {
	runbook := readDoc(t, "..", "..", "docs", "runbook.md")

	// The runbook's cron line: five schedule fields, then the command.
	// The fields are restricted to cron characters and separated by
	// spaces and tabs — \s would match a newline and let the pattern
	// span lines it has no business spanning.
	cronLine := regexp.MustCompile(`(?m)^([0-9*/,-]+[ \t]+[0-9*/,-]+[ \t]+[0-9*/,-]+[ \t]+[0-9*/,-]+[ \t]+[0-9*/,-]+)[ \t]+.*summarizer run`)
	m := cronLine.FindStringSubmatch(runbook)
	if m == nil {
		t.Fatal("docs/runbook.md has no cron line running summarizer")
	}
	want := strings.Join(strings.Fields(m[1])[:5], " ")

	schedule := regexp.MustCompile(`schedule:\s*"([^"]+)"`).FindStringSubmatch(weeklyManifest(t))
	if schedule == nil {
		t.Fatal("docs/kubernetes.md's weekly CronJob has no schedule")
	}
	if got := strings.Join(strings.Fields(schedule[1]), " "); got != want {
		t.Errorf("the Kubernetes weekly schedule is %q; the runbook's cron runs at %q. "+
			"One of them is reporting on the wrong week", got, want)
	}
}

// TestKubernetesDoc_ActiveDeadlineOutlastsTheRun: the binary bounds a
// run at six hours and ends it itself, printing the summary of what it
// published. The pod's deadline has to be longer, or Kubernetes kills
// the run at the same moment and the summary never appears — the
// operator learns that something failed and not what succeeded.
func TestKubernetesDoc_ActiveDeadlineOutlastsTheRun(t *testing.T) {
	m := regexp.MustCompile(`activeDeadlineSeconds:\s*(\d+)`).FindStringSubmatch(weeklyManifest(t))
	if m == nil {
		t.Fatal("docs/kubernetes.md's weekly CronJob has no activeDeadlineSeconds")
	}
	seconds, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("bad activeDeadlineSeconds %q", m[1])
	}
	if time.Duration(seconds)*time.Second <= runDeadline {
		t.Errorf("the pod's deadline is %ds, which is not longer than the run's own %s; "+
			"Kubernetes would kill the run before it printed its summary", seconds, runDeadline)
	}
}

// TestKubernetesDoc_ImageTagIsPublished: the same rule the README's
// image references obey — a tag the release does not push is a
// manifest that cannot be applied.
func TestKubernetesDoc_ImageTagIsPublished(t *testing.T) {
	published := publishedImageTags(t)
	doc := readDoc(t, "..", "..", "docs", "kubernetes.md")
	ref := regexp.MustCompile(`ghcr\.io/inful/summarizer:([A-Za-z0-9._-]+)`)
	found := false
	for _, m := range ref.FindAllStringSubmatch(doc, -1) {
		found = true
		tag := m[1]
		if !published[tag] && !strings.Contains(tag, "VERSION") {
			t.Errorf("docs/kubernetes.md references ghcr.io/inful/summarizer:%s, which the "+
				"release does not publish (it publishes %v)", tag, keys(published))
		}
	}
	if !found {
		t.Error("docs/kubernetes.md names no image; the manifest cannot be applied without one")
	}
}

// TestKubernetesDoc_ExitCodesAreReal: the table tells an operator what
// a failed pod means. A code the binary never returns is a warning
// about nothing.
func TestKubernetesDoc_ExitCodesAreReal(t *testing.T) {
	doc := readDoc(t, "..", "..", "docs", "kubernetes.md")
	row := regexp.MustCompile(`(?m)^\|\s*(\d)\s*\|`)
	found := false
	for _, m := range row.FindAllStringSubmatch(doc, -1) {
		found = true
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad code %q", m[1])
		}
		if _, ok := exitCodes[n]; !ok {
			t.Errorf("docs/kubernetes.md's table names exit %d, which the binary never returns", n)
		}
	}
	if !found {
		t.Error("docs/kubernetes.md has no exit-code table; a failed pod would be unexplainable")
	}
}
