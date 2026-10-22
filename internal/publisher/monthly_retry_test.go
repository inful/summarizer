package publisher

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(t *testing.T) {
	t.Helper()
	old := sleepFn
	sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { sleepFn = old })
}

func dropConn(w http.ResponseWriter) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_ = c.(*net.TCPConn).SetLinger(0) // RST, not FIN
	_ = c.Close()
}

// TestMonthlyReads_RetryTransientFailures: getStatus used to build its
// own request, so ListPeriodBranches and ReadReportFile had none of
// doRetry's policy. Measured against a server that recovers on its
// second attempt:
//
//	502 then 200        attempts=1  err=…: 502
//	dropped then 200    attempts=1  err=…: connection reset by peer
//
// A single connection reset aborted `summarizer monthly` outright — on a
// command that runs on the 1st, so a failure costs a month.
func TestMonthlyReads_RetryTransientFailures(t *testing.T) {
	noSleep(t)

	for _, fail := range []struct {
		name string
		fn   func(http.ResponseWriter)
	}{
		{"502", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }},
		{"429", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}},
		{"dropped connection", dropConn},
	} {
		t.Run(fail.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					fail.fn(w)
					return
				}
				w.Header().Set("X-Next-Page", "")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			g := &GitLabPublisher{
				Host: srv.URL, Project: "g/p", Token: "tok", DefaultBranch: "main",
				SourceBranchPrefix: "summarizer", HTTP: &http.Client{Timeout: 5 * time.Second},
			}
			if _, err := g.ListPeriodBranches(context.Background()); err != nil {
				t.Fatalf("ListPeriodBranches did not survive a %s: %v", fail.name, err)
			}
			if hits.Load() < 2 {
				t.Errorf("attempts = %d, want at least 2", hits.Load())
			}
		})
	}
}

// ReadReportFile takes the same getStatus, so it must get the same
// retry. A month is two reads per week, all bare before this.
func TestMonthlyReads_ReadReportFileRetriesTransientFailures(t *testing.T) {
	noSleep(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("## Weekly rollup")) + `"}`))
	}))
	defer srv.Close()

	g := &GitLabPublisher{
		Host: srv.URL, Project: "g/p", Token: "tok", DefaultBranch: "main",
		SourceBranchPrefix: "summarizer", HTTP: &http.Client{Timeout: 5 * time.Second},
	}
	body, err := g.ReadReportFile(context.Background(), "summarizer/2026-W42", "reports/2026-W42/x.md")
	if err != nil {
		t.Fatalf("ReadReportFile did not survive a 502: %v", err)
	}
	if body != "## Weekly rollup" {
		t.Errorf("body = %q, want the decoded report text", body)
	}
	if hits.Load() < 2 {
		t.Errorf("attempts = %d, want at least 2", hits.Load())
	}
}

// The transport retry is GET-only, and that boundary is the whole
// safety argument: a POST that lands and looks like a failure must NOT
// be repeated, or the publish duplicates a commit or an MR.
func TestDoRetry_TransportRetryIsGETOnly(t *testing.T) {
	noSleep(t)

	for _, tc := range []struct {
		method string
		retry  bool
		want   int32
		why    string
	}{
		// The server never recovers, so a retried GET exhausts its
		// attempts. That contrast is the point: writes stop at one.
		{http.MethodGet, true, maxRetries + 1, "a GET has no side effect to duplicate"},
		{http.MethodPost, true, 1, "a POST may have landed and still look like a failure"},
		{http.MethodPut, true, 1, "same for a PUT — the file write carries last_commit_id, " +
			"but the request may still have been applied"},
		{http.MethodPost, false, 1, "the merge-request create passes retry=false"},
	} {
		t.Run(tc.method+" retry="+map[bool]string{true: "true", false: "false"}[tc.retry], func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				dropConn(w)
			}))
			defer srv.Close()

			g := &GitLabPublisher{
				Host: srv.URL, Project: "g/p", Token: "tok",
				HTTP: &http.Client{Timeout: 5 * time.Second},
			}
			_, _, _, err := g.doRetry(context.Background(), srv.URL, tc.method, []byte(`{}`), tc.retry)
			if err == nil {
				t.Fatal("a dropped connection should surface an error")
			}
			if hits.Load() != tc.want {
				t.Errorf("attempts = %d, want %d — %s", hits.Load(), tc.want, tc.why)
			}
		})
	}
}
