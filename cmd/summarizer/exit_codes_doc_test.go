package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// exitCodes is every code the binary can return, with the name the
// runbook's table uses for it. The tables in README.md and
// docs/runbook.md are the operator's contract — a cron wrapper branches
// on them — and nothing else checks them against the code.
var exitCodes = map[int]string{
	exitOK:        "exitOK",
	exitUnknown:   "unclassified",
	exitUsage:     "exitUsage",
	exitConfig:    "exitConfig",
	exitGitLab:    "exitGitLab",
	exitLLM:       "exitLLM",
	exitWrite:     "exitWrite",
	exitEnv:       "exitEnv",
	exitInjection: "exitInjection",
}

// TestDocs_ExitCodeTablesMatchTheCode: the runbook's table said a
// prompt-injection rejection was exit 5, the README's said 8, and the
// code said 8 — `classify` maps it with errors.Is(err,
// run.ErrInjection) to exitInjection. The runbook also had no row for
// 8 at all, so its own copy-paste cron wrapper sent it to the
// "EXIT ABEND" arm.
//
// Documentation is not policed by anything, and that is how a table
// drifts from the constants it describes. This reads both tables and
// checks them against the code.
func TestDocs_ExitCodeTablesMatchTheCode(t *testing.T) {
	runbook := readDoc(t, "..", "..", "docs", "runbook.md")
	readme := readDoc(t, "..", "..", "README.md")

	// The runbook's table rows: "| 8 | `exitInjection` | ... |" and
	// "| 1 | *(unclassified)* | ... |".
	row := regexp.MustCompile(`(?m)^\|\s*(\d)\s*\|\s*([^|]+)\|`)
	got := map[int]string{}
	for _, m := range row.FindAllStringSubmatch(runbook, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad code %q", m[1])
		}
		got[n] = strings.Trim(strings.TrimSpace(m[2]), "`*()")
	}
	for code, name := range exitCodes {
		doc, ok := got[code]
		if !ok {
			t.Errorf("docs/runbook.md's exit table has no row for %d (%s)", code, name)
			continue
		}
		if !strings.EqualFold(doc, name) {
			t.Errorf("docs/runbook.md says exit %d is %q; the code calls it %s", code, doc, name)
		}
	}
	for code, name := range got {
		if _, ok := exitCodes[code]; !ok {
			t.Errorf("docs/runbook.md's exit table has a row for %d (%s), which the code never returns", code, name)
		}
	}

	// The README's table names no constants, so only the SET of codes
	// is checked there.
	readmeRow := regexp.MustCompile(`(?m)^\|\s*(\d)\s*\|`)
	readmeCodes := map[int]bool{}
	for _, m := range readmeRow.FindAllStringSubmatch(readme, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			readmeCodes[n] = true
		}
	}
	for code := range exitCodes {
		if !readmeCodes[code] {
			t.Errorf("README.md's exit table has no row for %d (%s)", code, exitCodes[code])
		}
	}
}

// TestDocs_RunbookCronWrapperCoversEveryExitCode: the runbook's
// copy-paste wrapper had `0|2|3|4|5|6|7` and sent 8 — a prompt the
// guard rejected as an injection attempt — to the generic EXIT ABEND
// arm. A security rejection that alerts as "EXIT ABEND" is the one
// outcome worth naming.
//
// exitUnknown (1) is deliberately NOT required to be listed: it is the
// "we don't know what this was" code, and the generic arm is its right
// home.
func TestDocs_RunbookCronWrapperCoversEveryExitCode(t *testing.T) {
	runbook := readDoc(t, "..", "..", "docs", "runbook.md")
	i := strings.Index(runbook, "case $? in")
	if i < 0 {
		t.Fatal("docs/runbook.md has no `case $? in` cron example")
	}
	rest := runbook[i:]

	// The first arm: a line of digits and pipes, then a closing paren.
	arm := regexp.MustCompile(`(?m)^\s*([0-9|]+)\)`).FindStringSubmatch(rest)
	if arm == nil {
		t.Fatal("the cron example has no `codes)` arm")
	}
	covered := map[int]bool{}
	for _, s := range strings.Split(arm[1], "|") {
		if n, err := strconv.Atoi(s); err == nil {
			covered[n] = true
		}
	}
	for code, name := range exitCodes {
		if code == exitUnknown {
			continue // the generic arm is its home
		}
		if code == exitInjection {
			// It needs its OWN arm, not the generic one — that is the
			// whole point of the row.
			if !regexp.MustCompile(`(?m)^\s*` + strconv.Itoa(code) + `\)\s`).MatchString(rest) {
				t.Errorf("the runbook's cron wrapper has no arm for exit %d (%s); "+
					"it would fall into the generic EXIT ABEND arm", code, name)
			}
			continue
		}
		if !covered[code] {
			t.Errorf("the runbook's cron wrapper does not handle exit %d (%s); "+
				"it would fall into the generic arm", code, name)
		}
	}
}

func readDoc(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %v: %v", parts, err)
	}
	return string(b)
}
