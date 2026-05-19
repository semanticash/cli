package llm

import "context"

// claudeWriter wraps the Claude Code CLI for use with the
// WriterRegistry fallback chain. Find() searches PATH plus the
// VS Code / VS Code Insiders extension bundle locations so users
// who installed Claude only through the IDE extension can still
// generate playbooks. Generate() invokes the CLI with
// --output-format json and parses the structured response.
type claudeWriter struct{}

// Claude returns a Writer for the Claude Code CLI. Used by the
// composition root to build the production WriterRegistry.
func Claude() Writer { return claudeWriter{} }

func (claudeWriter) Name() string  { return "claude_code" }
func (claudeWriter) Model() string { return defaultModel }
func (claudeWriter) Find() string  { return findClaude() }

func (claudeWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	return runClaude(ctx, binPath, prompt)
}
