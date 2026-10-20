package llm

import "strings"

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
