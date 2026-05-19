package llm

import "context"

// kiroCLIWriter wraps the Kiro CLI (`kiro-cli` binary) for use with
// the WriterRegistry fallback chain. Find() relies on PATH only.
// Kiro CLI routes status lines to stderr; Generate() reads the
// model response from stdout and strips the leading "> " prompt
// marker via cleanKiroCLIResponse.
type kiroCLIWriter struct{}

// KiroCLI returns a Writer for the Kiro CLI.
func KiroCLI() Writer { return kiroCLIWriter{} }

func (kiroCLIWriter) Name() string  { return "kiro_cli" }
func (kiroCLIWriter) Model() string { return "unknown" }
func (kiroCLIWriter) Find() string  { return findKiroCLI() }

func (kiroCLIWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	return runKiroCLI(ctx, binPath, prompt)
}
