package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	APIKey    string
	BaseURL   string // default https://api.anthropic.com
	Model     string // default claude-3-5-haiku-latest
	MaxTokens int    // default 2048
	HTTP      *http.Client
}

func NewAnthropic(apiKey, model string) *AnthropicProvider {
	return &AnthropicProvider{
		APIKey:    apiKey,
		BaseURL:   "https://api.anthropic.com",
		Model:     firstNonEmpty(model, "claude-3-5-haiku-latest"),
		MaxTokens: 2048,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

func (a *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{
		"model":      resolveModel(req.Model, a.Model),
		"system":     req.System,
		"messages":   []map[string]string{{"role": "user", "content": req.User}},
		"max_tokens": pickMaxTokens(req.MaxTokens, a.MaxTokens),
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return Response{}, fmt.Errorf("anthropic: %d %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Response{}, fmt.Errorf("anthropic: decode: %w", err)
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
	return Response{
		Text: text,
		Usage: Usage{
			PromptTokens:     out.Usage.InputTokens,
			CompletionTokens: out.Usage.OutputTokens,
			TotalTokens:      out.Usage.InputTokens + out.Usage.OutputTokens,
		},
	}, nil
}