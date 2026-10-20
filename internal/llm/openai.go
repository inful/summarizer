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

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	APIKey    string
	BaseURL   string // default https://api.openai.com/v1
	Model     string // default gpt-4o-mini
	MaxTokens int    // default 2048
	HTTP      *http.Client
}

func NewOpenAI(apiKey, model string) *OpenAIProvider {
	return &OpenAIProvider{
		APIKey:    apiKey,
		BaseURL:   "https://api.openai.com/v1",
		Model:     firstNonEmpty(model, "gpt-4o-mini"),
		MaxTokens: 2048,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{
		"model": resolveModel(req.Model, o.Model),
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"max_tokens": pickMaxTokens(req.MaxTokens, o.MaxTokens),
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return Response{}, fmt.Errorf("openai: %d %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Response{}, fmt.Errorf("openai: decode: %w", err)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return Response{}, ErrEmptyResponse
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
	return 1024
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}