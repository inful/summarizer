package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunCmd_ExitsBeforeAnyNetworkCall: runCmd is 171 lines and was the
// only function in this package with no test at all — every sibling
// (resolveLLM, newGuard, newPublisher, printSummary, classify,
// unionSlices, stringSliceFlag) is called from one. It is where the
// flag/config precedence, the discovery default and the six-stage
// wiring all live, and cmd/summarizer sits at 40% coverage, the lowest
// in the repo by a factor of two.
//
// None of that needs a network: with a token absent, the run must stop
// before constructing a client or calling GitLab.
func TestRunCmd_ExitsBeforeAnyNetworkCall(t *testing.T) {
	cfg := writeConfig(t, `
gitlab_host: "https://gitlab.example.com"
output_dir: `+t.TempDir()+`
`)
	// No GITLAB_TOKEN, and none of the LLM keys: whichever check comes
	// first, it must be an error and not a hang.
	for _, k := range []string{"GITLAB_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	if got := runCmd([]string{"--config", cfg}); got != exitEnv {
		t.Errorf("runCmd with no token = %d, want %d (exitEnv)", got, exitEnv)
	}
}

// TestRunCmd_FlagBeatsConfig: the precedence is the load-bearing part
// and it is invisible in a code read — six `if *flag != ""` blocks
// between config.Load and cfg.Validate(). Each one gets a case.
func TestRunCmd_FlagBeatsConfig(t *testing.T) {
	// The GitLab-token check runs before the LLM is resolved, so a
	// missing token would stop the run before the flag under test is
	// ever consulted. Set it; leave the LLM keys unset.
	t.Setenv("GITLAB_TOKEN", "glpat-test")
	dir := t.TempDir()
	cfg := writeConfig(t, `
gitlab_host: "https://from-config.example.com"
output_dir: `+dir+`
`)
	cases := []struct {
		name string
		args []string
		want string // must appear in the error the LLM resolution makes
	}{
		{"gitlab-host", []string{"--gitlab-host", "https://from-flag.example.com"}, "OPENAI_API_KEY"},
		{"provider", []string{"--provider", "anthropic"}, "ANTHROPIC_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Both providers' keys are unset, so whichever the config
			// settles on fails on its key — and the MESSAGE names the
			// key, which is how we tell which provider was selected.
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("ANTHROPIC_API_KEY", "")
			out := captureStderr(t, func() {
				if code := runCmd(append([]string{"--config", cfg}, c.args...)); code != exitEnv {
					t.Errorf("exit %d, want %d", code, exitEnv)
				}
			})
			if !strings.Contains(out, c.want) {
				t.Errorf("%s did not take effect; stderr did not mention %s:\n%s",
					c.name, c.want, out)
			}
		})
	}
}

// TestRunCmd_ConfigErrorsExitThree: a bad config must be exitConfig, not
// exitUsage or exitEnv, because a cron wrapper branches on the code.
func TestRunCmd_ConfigErrorsExitThree(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	cases := []struct {
		name string
		body string
	}{
		{"missing host", "output_dir: /tmp/x\n"},
		{"unknown provider", "gitlab_host: \"https://g.example.com\"\nprovider: gemini\n"},
		{"negative max_events", "gitlab_host: \"https://g.example.com\"\nmax_events: -1\n"},
		// max_events: 0 is NOT an error — Defaults() repairs it to 200
		// before Validate() runs, which is why this one passes:
		{"unknown key", "gitlab_host: \"https://g.example.com\"\nno_such_key: 1\n"},
		{"not yaml", "gitlab_host: [\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := writeConfig(t, c.body)
			if got := runCmd([]string{"--config", cfg}); got != exitConfig {
				t.Errorf("runCmd = %d, want %d (exitConfig)", got, exitConfig)
			}
		})
	}
}

// TestRunCmd_UsageErrorExitsTwo: a flag package error is exitUsage, and
// it is the only exit code that must not be confused with a config
// problem — both print to stderr.
func TestRunCmd_UsageErrorExitsTwo(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	cfg := writeConfig(t, "gitlab_host: \"https://g.example.com\"\n")
	for _, args := range [][]string{
		{"--config", cfg, "--no-such-flag"},
		{"--config", cfg, "--max-events", "not-a-number"},
	} {
		if got := runCmd(args); got != exitUsage {
			t.Errorf("runCmd(%v) = %d, want %d (exitUsage)", args, got, exitUsage)
		}
	}
}

// TestRunCmd_DefaultsToActiveDiscovery: with no discovery input the
// run falls back to --discover-active and SAYS SO. The line is what
// tells an operator why their run scanned the whole instance.
func TestRunCmd_DefaultsToActiveDiscovery(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	cfg := writeConfig(t, "gitlab_host: \"https://g.example.com\"\noutput_dir: "+t.TempDir()+"\n")
	// The exit code is irrelevant here: the run stops at the token check.
	fired := captureStderr(t, func() { runCmd([]string{"--config", cfg}) })
	if !strings.Contains(fired, "defaulting to --discover-active") {
		t.Errorf("the discovery default was applied silently:\n%s", fired)
	}

	// And it must NOT print that line when the operator named a source.
	named := captureStderr(t, func() {
		runCmd([]string{"--config", cfg, "--project", "group/a"})
	})
	if strings.Contains(named, "defaulting to --discover-active") {
		t.Errorf("claimed the default fired even though --project was given:\n%s", named)
	}
}

// TestRunCmd_PublishEnabledNeedsTokenAndProject: the misconfiguration
// monthly was missing for a long time. newPublisher is tested directly,
// but the path from a YAML file to that check is not — and the two
// failures have DIFFERENT codes, which is what a cron wrapper branches
// on: a missing token is exitEnv (7), a missing project is
// exitConfig (3).
func TestRunCmd_PublishEnabledNeedsTokenAndProject(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "glpat-test")
	body := `
gitlab_host: "https://gitlab.example.com"
publish:
  enabled: true
`
	t.Run("no reports token", func(t *testing.T) {
		t.Setenv("GITLAB_REPORTS_TOKEN", "")
		cfg := writeConfig(t, body)
		if got := runCmd([]string{"--config", cfg}); got != exitEnv {
			t.Errorf("exit %d, want %d (exitEnv): publish.enabled with no "+
				"GITLAB_REPORTS_TOKEN", got, exitEnv)
		}
	})
	t.Run("token but no project", func(t *testing.T) {
		t.Setenv("GITLAB_REPORTS_TOKEN", "glpat-reports")
		// resolveLLM runs before newPublisher, so the LLM key must be
		// present or the run stops one step short of the check under
		// test. A blank key is not enough — it has to be non-empty.
		t.Setenv("OPENAI_API_KEY", "sk-test")
		cfg := writeConfig(t, body)
		if got := runCmd([]string{"--config", cfg}); got != exitConfig {
			t.Errorf("exit %d, want %d (exitConfig): publish.enabled with a "+
				"token but no publish.project", got, exitConfig)
		}
	})
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summarizer.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestIncludeDiffsFlagHelpDoesNotClaimAnAPICall: the help said "adds one
// API call per MR". It adds none. fillMRDetail calls FetchMRDetail for
// every merge request unconditionally — wantDiffs only decides whether
// the returned chunks are kept — because prompt.Render needs the
// +N/-M in K files aggregate either way.
//
// The claim appeared in three places an operator reads (this help, the
// README caps table, the README prose) while the code comment above
// fillMRDetail and CHANGELOG.md:66 both said the opposite. Documentation
// is not policed by anything, so this pins the one sentence that made it
// actionable: the cost, stated as a cost.
func TestIncludeDiffsFlagHelpDoesNotClaimAnAPICall(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var help string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, `fs.Bool("include-diffs"`) {
			help = line
			break
		}
	}
	if help == "" {
		t.Fatal("could not find the --include-diffs flag declaration in main.go")
	}
	for _, claim := range []string{
		"adds one API call",
		"adds one extra",
		"one API call per MR",
	} {
		if strings.Contains(help, claim) {
			t.Errorf("the --include-diffs help still claims %q — the /changes call is "+
				"made either way, so the flag costs no requests at all:\n%s", claim, help)
		}
	}
	if !strings.Contains(help, "no API calls") {
		t.Errorf("the help should state the actual cost — that it adds none:\n%s", help)
	}
}
