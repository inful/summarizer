package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOpenAI_RetriesServerError: a transient 5xx must not abort the
// run. The GitLab client has retried 429/5xx three times since v0.1.0;
// the LLM providers did not, so one flaky completion killed a cron run
// that had already published files for earlier projects.
func TestOpenAI_RetriesServerError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-4o-mini", srv.URL)
	resp, err := o.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("should have retried past the 5xx: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d calls, want 3", got)
	}
}

// TestOpenAI_RetriesRateLimitHonoursRetryAfter: 429 must back off, not
// hammer. Retry-After is in seconds on the header; the client caps it
// so a hostile "Retry-After: 99999" can't park a cron run for a day.
func TestOpenAI_RetriesRateLimitHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", strconv.Itoa(1))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-4o-mini", srv.URL)
	start := time.Now()
	if _, err := o.Complete(context.Background(), Request{System: "s", User: "u"}); err != nil {
		t.Fatalf("should have retried past the 429: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("returned after %v; Retry-After: 1 was not honoured", elapsed)
	}
}

// TestOpenAI_GivesUpAfterMaxRetries: retrying forever would turn a
// broken endpoint into an infinite cron job.
func TestOpenAI_GivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-4o-mini", srv.URL)
	if _, err := o.Complete(context.Background(), Request{System: "s", User: "u"}); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	// maxRetries retries after the first attempt.
	if got := calls.Load(); got != int64(maxRetries)+1 {
		t.Errorf("made %d calls, want %d (first attempt + maxRetries)", got, maxRetries+1)
	}
}

// TestOpenAI_DoesNotRetryClientErrors: a 401 or 400 is our fault, not
// the endpoint's. Retrying just delays the operator's error message.
func TestOpenAI_DoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	o := NewOpenAI("bad-key", "gpt-4o-mini", srv.URL)
	if _, err := o.Complete(context.Background(), Request{System: "s", User: "u"}); err == nil {
		t.Fatal("expected an error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d calls on a 401, want 1 (no retry)", got)
	}
}

// TestOpenAI_ContextCancelsDuringBackoff: Ctrl-C on a cron run must
// stop the retry loop, not wait out the backoff.
func TestOpenAI_ContextCancelsDuringBackoff(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	o := NewOpenAI("k", "gpt-4o-mini", srv.URL)
	start := time.Now()
	_, err := o.Complete(ctx, Request{System: "s", User: "u"})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v to notice cancellation", elapsed)
	}
}

// TestAnthropic_RetriesServerError: same contract on the other provider.
func TestAnthropic_RetriesServerError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":4,"output_tokens":5}}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-3-5-haiku-latest", srv.URL)
	resp, err := a.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("should have retried past the 503: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
}

// TestAnthropic_NoAuthHeaderShape: the x-api-key + version headers are
// what Anthropic authenticates with; a regression here is a 401 on
// every run.
func TestAnthropic_HeaderShape(t *testing.T) {
	var gotKey, gotVersion, gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		body, _ := io.ReadAll(r.Body)
		gotSystem = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	a := NewAnthropic("sk-test", "claude-3-5-haiku-latest", srv.URL)
	if _, err := a.Complete(context.Background(), Request{System: "sysprompt", User: "u"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotKey != "sk-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if !strings.Contains(gotSystem, "sysprompt") {
		t.Errorf("system prompt not in the request body: %s", gotSystem)
	}
}
