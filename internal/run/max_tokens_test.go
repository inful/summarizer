package run

import (
	"context"
	"testing"
)

// TestRunOneProject_TheCompletionCapComesFromTheConfig: the completion
// cap is a config key, and it is the number that decides whether a
// busy week's report is finished or cut off mid-sentence. Every LLM
// call has to carry the operator's value, not a constant baked in at
// the call site — the constant is what made the cap unreachable.
func TestRunOneProject_TheCompletionCapComesFromTheConfig(t *testing.T) {
	p, llmStub := triageFixture(t, "")
	p.Cfg.MaxTokens = 12345

	if _, err := p.runOneProject(context.Background(), "group/public"); err != nil {
		t.Fatalf("runOneProject: %v", err)
	}

	llmStub.mu.Lock()
	defer llmStub.mu.Unlock()
	if len(llmStub.maxTokens) == 0 {
		t.Fatal("no LLM call was made")
	}
	for i, got := range llmStub.maxTokens {
		if got != 12345 {
			t.Errorf("call %d carried MaxTokens=%d, want the configured 12345", i, got)
		}
	}
}

// TestRunOneProject_TheDefaultCapIsTheDocumentedOne: with no config,
// the pipeline must send the default — the same number the llm package
// documents, so the two cannot drift into disagreeing about what a
// "default" completion cap is.
func TestRunOneProject_TheDefaultCapIsTheDocumentedOne(t *testing.T) {
	p, llmStub := triageFixture(t, "")
	p.Cfg.Defaults()

	if _, err := p.runOneProject(context.Background(), "group/public"); err != nil {
		t.Fatalf("runOneProject: %v", err)
	}

	llmStub.mu.Lock()
	defer llmStub.mu.Unlock()
	if len(llmStub.maxTokens) == 0 {
		t.Fatal("no LLM call was made")
	}
	for i, got := range llmStub.maxTokens {
		if got != p.Cfg.MaxTokens {
			t.Errorf("call %d carried MaxTokens=%d, want the default %d", i, got, p.Cfg.MaxTokens)
		}
	}
}
