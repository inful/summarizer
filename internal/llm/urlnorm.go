package llm

import (
	"net/url"
	"strings"
)

// normalizeBaseURL returns a base URL with no trailing slash and no trailing
// /v1 segment, so callers can append /v1/<endpoint> paths.
//
// Both forms are accepted from callers:
//   - https://host        → https://host
//   - https://host/       → https://host
//   - https://host/v1     → https://host
//   - https://host/v1/    → https://host
//
// Matches the convention used by OpenAI, Anthropic, Ollama, vLLM, LM
// Studio, llama.cpp, llama-stack — the base URL sometimes includes /v1 and
// sometimes does not. Same helper ragabast uses.
func normalizeBaseURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if len(trimmed) >= 3 && strings.EqualFold(trimmed[len(trimmed)-3:], "/v1") {
		trimmed = trimmed[:len(trimmed)-3]
	}
	return strings.TrimRight(trimmed, "/")
}

// IsOpenAIHost reports whether a configured base URL points at
// OpenAI's own API, where a key is mandatory. Callers use it to decide
// whether an empty key is a local-server convenience or a mistake —
// which is why it must not be a literal string compare: normalizeBaseURL
// accepts three spellings of the same host, and a guard that only
// matched one of them would let an operator send unauthenticated
// requests to api.openai.com for an entire run.
//
// It compares the ORIGIN, not the normalised string, because the path
// is not what decides where a request goes. normalizeBaseURL only
// strips a trailing /v1, so "https://api.openai.com/v1beta" used to
// miss — a host the operator plainly meant as OpenAI, misread as
// somebody's laptop, with the key treated as optional and the request
// posted with no Authorization header. The prompt in that request
// carries GitLab issue titles and bodies.
func IsOpenAIHost(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true // empty means the built-in default, which is OpenAI
	}
	u, err := url.Parse(normalizeBaseURL(raw))
	if err != nil {
		// Unparseable input is not going to work anyway; treat it as
		// the operator's own server rather than claiming a key is
		// required for a URL we cannot even read.
		return false
	}
	return strings.EqualFold(u.Host, "api.openai.com")
}
