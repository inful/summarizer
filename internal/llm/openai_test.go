package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAI_NoAuthHeaderWhenKeyEmpty: local OpenAI-compatible servers
// (Ollama, vLLM, LM Studio) don't require auth. Sending
// "Authorization: Bearer " with an empty key is rejected by some of
// them, so the client must omit the header entirely when APIKey == "".
func TestOpenAI_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		// Return a minimal valid OpenAI Chat Completions response.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	p := NewOpenAI("", "test-model", srv.URL)
	if _, err := p.Complete(context.Background(), Request{System: "s", User: "u"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sawAuth {
		t.Errorf("Authorization header was sent despite empty APIKey")
	}
}

// TestOpenAI_AuthHeaderWhenKeySet: the opposite — when the key IS set,
// the header must be present so the canonical api.openai.com rejects
// unauthenticated calls.
func TestOpenAI_AuthHeaderWhenKeySet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	p := NewOpenAI("sk-test", "test-model", srv.URL)
	if _, err := p.Complete(context.Background(), Request{System: "s", User: "u"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("expected Authorization: Bearer ..., got %q", gotAuth)
	}
}