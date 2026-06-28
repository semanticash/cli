package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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
//
// timeouts is a per-writer override of the shared 120s shell timeout.
// A workload that wants tighter caps (e.g. intent-gap, where Claude
// timing out at 120s blocks a faster downstream writer) sets these
// per-name; everything not in the map uses the writer's own default.
type WriterRegistry struct {
	writers  []Writer
	redactor func(string) (string, error)
	timeouts map[string]time.Duration
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

// NewWriterRegistryWithTimeouts is like NewWriterRegistry but applies a
// per-writer-name shell timeout override. Names not in the map fall
// back to the writer's own default (currently the shared 120s cap).
// Used by workload-specific registries that want to bound how long a
// failing writer can hold up the fallback chain.
func NewWriterRegistryWithTimeouts(writers []Writer, timeouts map[string]time.Duration) *WriterRegistry {
	return &WriterRegistry{
		writers:  writers,
		redactor: redactPrompt,
		timeouts: timeouts,
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
	// Several writer CLIs (codex, cursor, claude) misbehave on
	// non-UTF-8 input: codex prints an explicit "input is not valid
	// UTF-8" error and exits; cursor exits silently; claude reads the
	// stream and stalls until the deadline. Normalize once here so a
	// stray byte from upstream rendering cannot turn into minutes of
	// silent fallback latency.
	sanitized, badCount, badOffsets := sanitizeUTF8(redacted)
	redacted = sanitized

	var lastErr error
	var tried int
	var fallbackErrors []string

	for _, w := range r.writers {
		binPath := w.Find()
		if binPath == "" {
			continue
		}
		tried++

		text, err := r.callWriter(ctx, w, binPath, redacted)
		if err != nil {
			lastErr = chainErr(lastErr, w.Name(), err)
			fallbackErrors = append(fallbackErrors, formatFallbackError(w.Name(), err))
			continue
		}

		return &GenerateTextResult{
			Text:                 text,
			Provider:             w.Name(),
			Model:                w.Model(),
			FallbackErrors:       fallbackErrors,
			PromptBadByteCount:   badCount,
			PromptBadByteOffsets: badOffsets,
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed: %w", lastErr)
	}
	return nil, r.notInstalledError()
}

// callWriter applies any per-writer timeout override before invoking
// the writer's Generate. The override is enforced via ctx.WithTimeout,
// so the writer's own internal timeout becomes the cap when no
// override is configured. Names not in the timeouts map keep their
// default behavior.
func (r *WriterRegistry) callWriter(ctx context.Context, w Writer, binPath, prompt string) (string, error) {
	if t, ok := r.timeouts[w.Name()]; ok && t > 0 {
		callCtx, cancel := context.WithTimeout(ctx, t)
		defer cancel()
		return w.Generate(callCtx, binPath, prompt)
	}
	return w.Generate(ctx, binPath, prompt)
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

		text, err := r.callWriter(ctx, w, binPath, redacted)
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

// formatFallbackError builds the per-writer diagnostic line surfaced in
// GenerateTextResult.FallbackErrors. The error text is truncated so a
// chatty subprocess message does not bloat the local activity log.
func formatFallbackError(writerName string, err error) string {
	const maxErrBytes = 300
	msg := err.Error()
	if len(msg) > maxErrBytes {
		msg = msg[:maxErrBytes] + "...(truncated)"
	}
	return writerName + ": " + msg
}

// sanitizeUTF8 replaces every invalid UTF-8 byte in the prompt with the
// Unicode replacement character U+FFFD. Returns the normalized prompt,
// the total number of bytes replaced, and the byte offsets of the
// first 10 replacements so a downstream caller can locate the source
// renderer producing the bad input.
//
// The function never errors: a prompt that is already valid UTF-8 is
// returned unchanged with zero counts. The offsets are reported in the
// original (pre-replacement) input so a developer can index into the
// raw rendered prompt to find the offending byte.
func sanitizeUTF8(s string) (string, int, []int) {
	if utf8.ValidString(s) {
		return s, 0, nil
	}
	const maxOffsets = 10
	var out strings.Builder
	out.Grow(len(s))
	offsets := make([]int, 0, maxOffsets)
	count := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if len(offsets) < maxOffsets {
				offsets = append(offsets, i)
			}
			count++
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		out.WriteString(s[i : i+size])
		i += size
	}
	return out.String(), count, offsets
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
