package llm

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com", "https://api.openai.com"},
		{"https://api.openai.com/", "https://api.openai.com"},
		{"https://api.openai.com/v1", "https://api.openai.com"},
		{"https://api.openai.com/v1/", "https://api.openai.com"},
		{"https://api.anthropic.com", "https://api.anthropic.com"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com"},
		{"https://example.com/v1/chat/completions", "https://example.com/v1/chat/completions"}, // /v1 not trailing, untouched
		{"http://localhost:11434", "http://localhost:11434"},                                   // Ollama default
		{"http://localhost:11434/v1", "http://localhost:11434"},
	}
	for _, c := range cases {
		got := normalizeBaseURL(c.in)
		if got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
