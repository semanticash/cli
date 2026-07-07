package intentgap

import (
	"context"
	"errors"
	"sync"

	"github.com/semanticash/cli/internal/llm"
)

// fakeLLMRunner returns a canned sequence of responses, one per call.
// Each call advances the cursor; tests pin which response shape the
// caller is exercising (first attempt only vs first attempt + retry).
// Safe under concurrent GenerateText calls so parallel worker pools
// share one runner without races.
type fakeLLMRunner struct {
	mu        sync.Mutex
	responses []*llm.GenerateTextResult
	errs      []error
	calls     int
	prompts   []string
}

func (f *fakeLLMRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.prompts = append(f.prompts, prompt)
	var (
		res *llm.GenerateTextResult
		err error
	)
	if idx >= len(f.responses) {
		err = errors.New("fakeLLMRunner: ran out of canned responses")
	} else if idx < len(f.errs) && f.errs[idx] != nil {
		err = f.errs[idx]
	} else {
		res = f.responses[idx]
	}
	f.mu.Unlock()
	return res, err
}

