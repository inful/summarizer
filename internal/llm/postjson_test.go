package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPostJSON_BoundsTheResponseRead: this is the one place the process
// talks to a host it does not control. An unbounded io.ReadAll there is
// a remote-triggered OOM — and on the error path the body is truncated
// to 300 bytes anyway, so the full read bought nothing.
func TestPostJSON_BoundsTheResponseRead(t *testing.T) {
	// 3x the cap, so an unbounded read would clearly exceed it.
	flood := strings.Repeat("A", maxResponseBytes*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Status first: WriteHeader after a Write is a no-op.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(flood))
	}))
	defer srv.Close()

	hc := &http.Client{Timeout: 60 * time.Second}
	var out struct{}
	err := postJSON(context.Background(), hc, srv.URL, "test",
		map[string]string{}, map[string]any{"x": 1}, &out)

	if err == nil {
		t.Fatal("expected a status error")
	}
	// The bound is the point: the message carries only a truncated body
	// no matter how much the server sent.
	if len(err.Error()) > 400 {
		t.Errorf("error message should carry only a truncated body, got %d chars", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "test: 500") {
		t.Errorf("error should name the provider and status, got %q", err.Error())
	}
}

// TestPostJSON_SmallResponsesUnaffected: the bound must not truncate a
// normal response. A 2048-token completion is roughly 8 KB, two orders
// of magnitude below the cap.
func TestPostJSON_SmallResponsesUnaffected(t *testing.T) {
	want := strings.Repeat("hello world ", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + want + `"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAI("k", "m", srv.URL)
	p.BaseURL = srv.URL
	resp, err := p.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Text) != len(want) {
		t.Errorf("response truncated: got %d chars, want %d", len(resp.Text), len(want))
	}
}

// TestPostJSON_RetriesOnServerError: the shared helper must keep the
// retry policy both providers had.
func TestPostJSON_RetriesOnServerError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAI("k", "m", srv.URL)
	p.BaseURL = srv.URL
	resp, err := p.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "recovered" {
		t.Errorf("got %q, want %q", resp.Text, "recovered")
	}
	if hits != 3 {
		t.Errorf("expected 3 attempts, got %d", hits)
	}
}

// TestProviders_SendTheirOwnAuthHeaders: merging the two Complete
// methods must not blur the difference between them. OpenAI omits an
// empty Authorization so local servers accept it; Anthropic always
// sends x-api-key with the version header.
func TestProviders_SendTheirOwnAuthHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "messages") {
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	o := NewOpenAI("", "m", srv.URL)
	o.BaseURL = srv.URL
	if _, err := o.Complete(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["Authorization"]; ok {
		t.Error("openai: must omit Authorization when no key is configured (local servers reject an empty bearer)")
	}

	a := NewAnthropic("", "m", srv.URL)
	a.BaseURL = srv.URL
	if _, err := a.Complete(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if got.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("anthropic: missing version header, got %q", got.Get("Anthropic-Version"))
	}
	if _, ok := got["X-Api-Key"]; !ok {
		t.Error("anthropic: x-api-key header must always be sent")
	}
}
