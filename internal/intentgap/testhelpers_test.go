package intentgap

import (
	"context"
	"errors"

	"github.com/semanticash/cli/internal/llm"
)

// fakeLLMRunner returns a canned sequence of responses, one per call.
// Each call advances the cursor; tests pin which response shape the
// caller is exercising (first attempt only vs first attempt + retry).
type fakeLLMRunner struct {
	responses []*llm.GenerateTextResult
	errs      []error
	calls     int
	prompts   []string
}

func (f *fakeLLMRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	idx := f.calls
	f.calls++
	f.prompts = append(f.prompts, prompt)
	if idx >= len(f.responses) {
		return nil, errors.New("fakeLLMRunner: ran out of canned responses")
	}
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	return f.responses[idx], nil
}

func canned(text string) *llm.GenerateTextResult {
	return &llm.GenerateTextResult{Text: text, Provider: "claude_code", Model: "claude-opus-4-7"}
}
