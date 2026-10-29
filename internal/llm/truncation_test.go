package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// completeFn is the method value both providers expose.
type completeFn func(context.Context, Request) (Response, error)

// TestComplete_TruncationErrorReportsTheCapThatWasSent: the error has
// to name the cap the request carried, not the adapter's default.
//
// The completion cap is an operator-facing config key now, so the two
// numbers differ whenever the operator sets one: an operator who set
// max_tokens: 1000 and read "of 4096 tokens" would be told nothing
// about why their report was cut off, and would conclude the wrong
// thing. Before the cap was configurable the two numbers were always
// the same, so this was accidentally right.
func TestComplete_TruncationErrorReportsTheCapThatWasSent(t *testing.T) {
	cases := []struct {
		name     string
		provider func(baseURL string) completeFn
		body     string
	}{
		{
			name:     "openai",
			provider: func(u string) completeFn { return NewOpenAI("k", "gpt-4o", u).Complete },
			body: `{"choices":[{"finish_reason":"length","message":{"content":"cut off"}}],` +
				`"usage":{"prompt_tokens":100,"completion_tokens":1000,"total_tokens":1100}}`,
		},
		{
			name:     "anthropic",
			provider: func(u string) completeFn { return NewAnthropic("k", "claude", u).Complete },
			body: `{"stop_reason":"max_tokens","content":[{"type":"text","text":"cut off"}],` +
				`"usage":{"input_tokens":100,"output_tokens":1000}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			_, err := c.provider(srv.URL)(context.Background(), Request{
				System: "s", User: "u", MaxTokens: 1000,
			})
			if err == nil {
				t.Fatal("a truncated response must error")
			}
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("err = %v, want ErrTruncated", err)
			}
			if !strings.Contains(err.Error(), "of 1000 tokens") {
				t.Errorf("the error names the wrong cap: %v", err)
			}
		})
	}
}

// TestComplete_TruncatedResponseIsAnError: a model that runs into
// max_tokens is stopped mid-sentence, and the provider hands back
// perfectly valid partial content. Every check this package used to
// make — 2xx, decodable, non-empty — passes on it, so the pipeline
// published "…The deploy pipeline is still re" as a finished customer
// report: public: true, committed, and merged by a human who sees a
// normal-looking file.
//
// A clean finish must still pass, or the check would fire on every call.
func TestComplete_TruncatedResponseIsAnError(t *testing.T) {
	const fragment = `## Week in review\n\nWe shipped the auth\n\n## Blockers\n- The deploy pipeline is still re`

	cases := []struct {
		name     string
		provider func(baseURL string) completeFn
		body     string
		wantErr  bool
	}{
		{
			name:     "openai hit the cap",
			provider: func(u string) completeFn { return NewOpenAI("k", "gpt-4o", u).Complete },
			body: `{"choices":[{"finish_reason":"length","message":{"content":"` + fragment + `"}}],` +
				`"usage":{"prompt_tokens":9000,"completion_tokens":2048,"total_tokens":11048}}`,
			wantErr: true,
		},
		{
			name:     "openai finished",
			provider: func(u string) completeFn { return NewOpenAI("k", "gpt-4o", u).Complete },
			body: `{"choices":[{"finish_reason":"stop","message":{"content":"A complete report."}}],` +
				`"usage":{"prompt_tokens":9000,"completion_tokens":12,"total_tokens":9012}}`,
		},
		{
			name:     "anthropic hit the cap",
			provider: func(u string) completeFn { return NewAnthropic("k", "claude", u).Complete },
			body: `{"stop_reason":"max_tokens","content":[{"type":"text","text":"` + fragment + `"}],` +
				`"usage":{"input_tokens":9000,"output_tokens":2048}}`,
			wantErr: true,
		},
		{
			name:     "anthropic finished",
			provider: func(u string) completeFn { return NewAnthropic("k", "claude", u).Complete },
			body: `{"stop_reason":"end_turn","content":[{"type":"text","text":"A complete report."}],` +
				`"usage":{"input_tokens":9000,"output_tokens":12}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			resp, err := c.provider(srv.URL)(context.Background(), Request{
				System: "s", User: "u", MaxTokens: 2048,
			})
			if !c.wantErr {
				if err != nil {
					t.Fatalf("a clean finish must not error: %v", err)
				}
				if resp.Text != "A complete report." {
					t.Errorf("text = %q", resp.Text)
				}
				return
			}
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("err = %v, want ErrTruncated; the fragment came back as text: %q",
					err, resp.Text)
			}
			if resp.Text != "" {
				t.Errorf("a truncated response must not carry text: %q", resp.Text)
			}
		})
	}
}
