package llm

import "context"

// cursorWriter wraps the Cursor CLI (`agent` binary) for use with
// the WriterRegistry fallback chain. Find() searches PATH plus the
// per-user Cursor install directory under ~/.cursor/bin/ so a
// laptop with Cursor installed via the IDE but no PATH entry still
// resolves.
type cursorWriter struct{}

// Cursor returns a Writer for the Cursor agent CLI.
func Cursor() Writer { return cursorWriter{} }

func (cursorWriter) Name() string  { return "cursor" }
func (cursorWriter) Model() string { return "unknown" }
func (cursorWriter) Find() string  { return findCursorAgent() }

func (cursorWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	return runCursor(ctx, binPath, prompt)
}
