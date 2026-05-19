package llm

import "context"

// copilotWriter wraps the GitHub Copilot CLI for use with the
// WriterRegistry fallback chain. Find() relies on PATH only.
type copilotWriter struct{}

// Copilot returns a Writer for the GitHub Copilot CLI.
func Copilot() Writer { return copilotWriter{} }

func (copilotWriter) Name() string  { return "copilot" }
func (copilotWriter) Model() string { return "unknown" }
func (copilotWriter) Find() string  { return findCopilot() }

func (copilotWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	return runCopilot(ctx, binPath, prompt)
}
