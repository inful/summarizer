// Package llm defines a Provider interface and ships OpenAI and Anthropic
// adapters. Both providers are net/http POST + bearer + JSON — no SDK.
package llm

import (
	"context"
	"errors"
)

// Provider is the LLM port the pipeline uses. Adapters in this package
// implement it. Adding a third provider is a new file + a constructor.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Request is the LLM-agnostic input. System carries persistent instructions
// (kept short and outside the events payload); User carries the rendered
// prompt (which contains <data>-wrapped event content).
type Request struct {
	System    string
	User      string
	Model     string
	MaxTokens int // caps the response length; LLM-appropriate defaults chosen per provider
}

// Response is the LLM-agnostic output. Text is what to write to disk; Usage
// is for the run summary line on stderr.
type Response struct {
	Text  string
	Usage Usage
}

// Usage is provider-normalized token accounting. Not all providers surface
// the same fields; fields the provider doesn't return are zero.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ErrEmptyResponse signals the provider returned a 2xx with no usable text.
// Callers should refuse the file write and exit non-zero — never write a
// half-empty report.
var ErrEmptyResponse = errors.New("llm returned empty response")

// maxResponseBytes caps how much of a response body is read into memory.
// This is the one place the process talks to a host it does not control,
// so an unbounded io.ReadAll is a remote-triggered OOM. 8 MB is orders
// of magnitude above any Chat Completions response (a 2048-token
// completion is roughly 8 KB) while still bounding a malfunctioning or
// hostile endpoint.
const maxResponseBytes = 8 << 20

// resolveModel picks the actual model id to call. req.Model wins; the
// adapter's default fills in if empty.
func resolveModel(req, def string) string {
	if req != "" {
		return req
	}
	return def
}
