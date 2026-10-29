package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/inful/summarizer/internal/textutil"
)

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	APIKey    string
	BaseURL   string // default https://api.openai.com/v1
	Model     string // default gpt-4o-mini
	MaxTokens int    // default DefaultMaxTokens
	HTTP      *http.Client
}

func NewOpenAI(apiKey, model, baseURL string) *OpenAIProvider {
	return &OpenAIProvider{
		APIKey:    apiKey,
		BaseURL:   normalizeBaseURL(firstNonEmpty(baseURL, "https://api.openai.com/v1")),
		Model:     firstNonEmpty(model, "gpt-4o-mini"),
		MaxTokens: DefaultMaxTokens,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Complete posts to the Chat Completions endpoint and decodes the
// choice's content.
func (o *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{
		"model": resolveModel(req.Model, o.Model),
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"max_tokens": pickMaxTokens(req.MaxTokens, o.MaxTokens),
	}
	headers := map[string]string{"Content-Type": "application/json"}
	// Local OpenAI-compatible servers (Ollama, vLLM, LM Studio) usually
	// don't require auth. Sending "Authorization: Bearer " with an empty
	// key is rejected by some of them, so omit the header entirely when
	// no key is configured.
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := postJSON(ctx, o.HTTP, o.BaseURL+"/v1/chat/completions", "openai", headers, body, &out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return Response{}, ErrEmptyResponse
	}
	// Checked before the content is used, not after. A cap-hit response
	// is present and non-empty, so nothing else in this function would
	// notice that it stops mid-sentence.
	if out.Choices[0].FinishReason == "length" {
		return Response{}, fmt.Errorf("%w (%d of %d tokens)",
			ErrTruncated, out.Usage.CompletionTokens, o.MaxTokens)
	}
	return Response{
		Text: out.Choices[0].Message.Content,
		Usage: Usage{
			PromptTokens:     out.Usage.PromptTokens,
			CompletionTokens: out.Usage.CompletionTokens,
			TotalTokens:      out.Usage.TotalTokens,
		},
	}, nil
}

// postJSON is the shared request half of both providers: marshal,
// POST with retry, bound the response read, surface a status error with
// the body quoted, then decode. Only the URL, the headers, the request
// body and the response shape differ between providers — everything else
// was duplicated, and the copies had already drifted once (OpenAI omits
// an empty Authorization header so local servers accept it; Anthropic
// always sends x-api-key).
func postJSON(ctx context.Context, hc *http.Client, url, provider string, headers map[string]string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode: %w", provider, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := doWithRetry(ctx, hc, httpReq, buf)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded: this reads from a host we do not control, and the error
	// path only keeps the first 300 bytes anyway.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %d %s", provider, resp.StatusCode, textutil.Truncate(string(respBody), 300))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s: decode: %w", provider, err)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pickMaxTokens(req, def int) int {
	if req > 0 {
		return req
	}
	if def > 0 {
		return def
	}
	return DefaultMaxTokens
}
