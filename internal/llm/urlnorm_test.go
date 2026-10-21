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

// TestIsOpenAIHost: the guard that decides "is a key mandatory" must
// match every spelling normalizeBaseURL accepts. It used to compare
// against one literal, so `openai_base_url: https://api.openai.com` —
// a form the config comment says is supported — sent an unauthenticated
// request to api.openai.com for an entire run instead of failing at
// start-up.
func TestIsOpenAIHost(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://api.openai.com", true},
		{"https://api.openai.com/v1/", true},
		{"https://API.OpenAI.com/v1", true},
		{"", true}, // the built-in default
		{"http://localhost:11434/v1", false},
		{"http://localhost:8000/v1", false},
		{"https://openrouter.ai/api/v1", false},
	} {
		if got := IsOpenAIHost(tc.raw); got != tc.want {
			t.Errorf("IsOpenAIHost(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
