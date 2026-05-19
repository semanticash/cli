package llm

import (
	"context"
	"fmt"
	"strings"
)

// Writer is the per-LLM CLI integration that the WriterRegistry walks
// in fallback order. Each writer locates its binary, generates a
// response, and reports its model and display name independently. The
// registry owns redaction, ordering, and the fallback chain; writers
// own the subprocess-level details of how their CLI is invoked.
type Writer interface {
	// Name is the stable identifier used in logs, attribution
	// records, and error messages (e.g. "claude_code", "codex").
	Name() string
	// Model reports the model the writer's CLI will invoke. "unknown"
	// is acceptable for CLIs that don't expose a stable model alias.
	Model() string
	// Find returns an executable path for this writer's CLI, or
	// empty when the binary is not installed on the host. Must be
	// cross-platform (PATH lookup honors .exe on Windows etc.).
	Find() string
	// Generate sends prompt to the writer's CLI subprocess and
	// returns the model's response as text. binPath is the resolved
	// binary location from a prior Find() call. Returns an error
	// the registry uses to fall through to the next writer.
	Generate(ctx context.Context, binPath, prompt string) (string, error)
}

// WriterRegistry holds the ordered list of writers tried by Generate
// and GenerateText. The fallback contract is: redact once, try each
// writer with a non-empty Find() in order, return the first success;
// when every writer either skips (no binary) or errors, return a
// chained error naming each failure in fallback order.
//
// Redaction is performed once before any writer sees the prompt. The
// redactor field is unexported and seamable from tests in the same
// package; callers outside the package always get the production
// redactPrompt.
type WriterRegistry struct {
	writers  []Writer
	redactor func(string) (string, error)
}

// NewWriterRegistry constructs a registry over the given writers in
// fallback order. Order matters: the first writer with an available
// CLI that returns a successful response wins. Production wiring
// lives in internal/providers/composition.go.
func NewWriterRegistry(writers ...Writer) *WriterRegistry {
	return &WriterRegistry{
		writers:  writers,
		redactor: redactPrompt,
	}
}

// List returns the registered writers in fallback order. Used by the
// "no AI CLI found" error message to enumerate install candidates,
// and by health checks that want to inspect the registry shape.
func (r *WriterRegistry) List() []Writer {
	out := make([]Writer, len(r.writers))
	copy(out, r.writers)
	return out
}

// GenerateText sends a redacted prompt to the first available writer
// and returns its raw text response. The redaction step runs exactly
// once regardless of how many writers are tried. Writers whose Find()
// returns empty are skipped (no subprocess attempt). Writers whose
// Generate returns an error fall through to the next writer; their
// error is appended to a chained message returned only when every
// writer fails. When no writer is even installed, returns a single-
// line "no AI CLI found" message that enumerates every registered
// writer so the install hint stays honest as the registry grows.
func (r *WriterRegistry) GenerateText(ctx context.Context, prompt string) (*GenerateTextResult, error) {
	redacted, err := r.redactor(prompt)
	if err != nil {
		return nil, fmt.Errorf("egress redaction failed: %w", err)
	}

	var lastErr error
	var tried int

	for _, w := range r.writers {
		binPath := w.Find()
		if binPath == "" {
			continue
		}
		tried++

		text, err := w.Generate(ctx, binPath, redacted)
		if err != nil {
			lastErr = chainErr(lastErr, w.Name(), err)
			continue
		}

		return &GenerateTextResult{
			Text:     text,
			Provider: w.Name(),
			Model:    w.Model(),
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed: %w", lastErr)
	}
	return nil, r.notInstalledError()
}

// Generate is the narrative variant: same fallback contract as
// GenerateText, but each writer's text response is parsed as a
// NarrativeResult JSON blob before being returned. A successful CLI
// response that fails to parse as a narrative is treated as a writer
// failure (falls through to the next writer); the parse error is
// chained into the final message.
func (r *WriterRegistry) Generate(ctx context.Context, prompt string) (*GenerateResult, error) {
	redacted, err := r.redactor(prompt)
	if err != nil {
		return nil, fmt.Errorf("egress redaction failed: %w", err)
	}

	var lastErr error
	var tried int

	for _, w := range r.writers {
		binPath := w.Find()
		if binPath == "" {
			continue
		}
		tried++

		text, err := w.Generate(ctx, binPath, redacted)
		if err != nil {
			lastErr = chainErr(lastErr, w.Name(), err)
			continue
		}

		narrative, err := parseNarrativeJSON(text)
		if err != nil {
			lastErr = chainErr(lastErr, w.Name(), err)
			continue
		}

		return &GenerateResult{
			Narrative: narrative,
			Provider:  w.Name(),
			Model:     w.Model(),
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed: %w", lastErr)
	}
	return nil, r.notInstalledError()
}

// chainErr appends a writer-named failure to the running error chain.
// Mirrors the pre-registry fallback error format so log readers see
// the same shape across the transition.
func chainErr(prev error, writerName string, err error) error {
	if prev != nil {
		return fmt.Errorf("%s: %w (after %v)", writerName, err, prev)
	}
	return fmt.Errorf("%s: %w", writerName, err)
}

// notInstalledError builds the terminal error returned when no
// registered writer has its CLI installed. The install hint is
// derived from the registry's display names rather than hardcoded,
// so adding a new writer (Codex, future LLMs) keeps the message
// honest without a manual edit.
func (r *WriterRegistry) notInstalledError() error {
	names := make([]string, 0, len(r.writers))
	for _, w := range r.writers {
		names = append(names, displayNameFor(w.Name()))
	}
	if len(names) == 0 {
		return fmt.Errorf("no AI CLI found")
	}
	return fmt.Errorf("no AI CLI found. Install %s", strings.Join(names, ", "))
}

// displayNameFor maps a writer's stable Name() (used in attribution
// records, e.g. "claude_code") to a human-readable install hint
// (e.g. "Claude Code"). Returns the input unchanged when no mapping
// exists, so new writers automatically participate even before this
// table is updated.
func displayNameFor(name string) string {
	switch name {
	case "claude_code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "cursor":
		return "Cursor"
	case "gemini_cli":
		return "Gemini CLI"
	case "copilot":
		return "GitHub Copilot CLI"
	case "kiro_cli":
		return "Kiro CLI"
	}
	return name
}
