package llm

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	APIKey    string
	BaseURL   string // default https://api.anthropic.com
	Model     string // default claude-3-5-haiku-latest
	MaxTokens int    // default DefaultMaxTokens
	HTTP      *http.Client
}

func NewAnthropic(apiKey, model, baseURL string) *AnthropicProvider {
	return &AnthropicProvider{
		APIKey:    apiKey,
		BaseURL:   normalizeBaseURL(firstNonEmpty(baseURL, "https://api.anthropic.com")),
		Model:     firstNonEmpty(model, "claude-3-5-haiku-latest"),
		MaxTokens: DefaultMaxTokens,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Complete posts to the Messages endpoint and concatenates every text
// block in the reply.
func (a *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	// The cap this call actually uses — see the same note in the OpenAI
	// adapter: the truncation error has to report the number the request
	// body carried, not the adapter's default.
	maxTokens := pickMaxTokens(req.MaxTokens, a.MaxTokens)
	body := map[string]any{
		"model":      resolveModel(req.Model, a.Model),
		"system":     req.System,
		"messages":   []map[string]string{{"role": "user", "content": req.User}},
		"max_tokens": maxTokens,
	}
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         a.APIKey,
		"anthropic-version": "2023-06-01",
	}

	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := postJSON(ctx, a.HTTP, a.BaseURL+"/v1/messages", "anthropic", headers, body, &out); err != nil {
		return Response{}, err
	}
	var text string
	for _, c := range out.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return Response{}, ErrEmptyResponse
	}
	// See ErrTruncated: a cap-hit response is non-empty and decodes
	// cleanly, so without this the pipeline publishes a fragment as a
	// finished customer report.
	if out.StopReason == "max_tokens" {
		return Response{}, fmt.Errorf("%w (%d of %d tokens)",
			ErrTruncated, out.Usage.OutputTokens, maxTokens)
	}
	return Response{
		Text: text,
		Usage: Usage{
			PromptTokens:     out.Usage.InputTokens,
			CompletionTokens: out.Usage.OutputTokens,
			TotalTokens:      out.Usage.InputTokens + out.Usage.OutputTokens,
		},
	}, nil
}
