package llm

import "context"

// geminiWriter wraps the Gemini CLI for use with the
// WriterRegistry fallback chain. Find() relies on PATH only:
// gemini ships as a single binary with no well-known per-user
// install location to fall back to.
type geminiWriter struct{}

// Gemini returns a Writer for the Gemini CLI.
func Gemini() Writer { return geminiWriter{} }

func (geminiWriter) Name() string  { return "gemini_cli" }
func (geminiWriter) Model() string { return "unknown" }
func (geminiWriter) Find() string  { return findGemini() }

func (geminiWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	return runGemini(ctx, binPath, prompt)
}
