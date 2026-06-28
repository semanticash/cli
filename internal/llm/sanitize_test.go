package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// slowWriter blocks in Generate until the provided context expires or
// the configured delay elapses. Used to exercise per-writer timeout
// enforcement at the registry layer.
type slowWriter struct {
	name  string
	delay time.Duration
}

func (s *slowWriter) Name() string  { return s.name }
func (s *slowWriter) Model() string { return "test" }
func (s *slowWriter) Find() string  { return "/usr/bin/true" }
func (s *slowWriter) Generate(ctx context.Context, _, _ string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(s.delay):
		return "ok", nil
	}
}

// failingWriter always errors so the registry walks past it. Find()
// returns a non-empty path so the registry treats it as installed.
type failingWriter struct {
	name string
	err  error
}

func (f *failingWriter) Name() string  { return f.name }
func (f *failingWriter) Model() string { return "test" }
func (f *failingWriter) Find() string  { return "/usr/bin/true" }
func (f *failingWriter) Generate(_ context.Context, _, _ string) (string, error) {
	return "", f.err
}

// A prompt with valid UTF-8 round-trips unchanged and the result
// reports zero replacements.
func TestSanitizeUTF8_ValidStringUnchanged(t *testing.T) {
	in := "hello world\nwith newlines\nand UTF-8: café résumé naïve"
	out, count, offsets := sanitizeUTF8(in)
	if out != in {
		t.Errorf("valid UTF-8 was modified:\n  in:  %q\n  out: %q", in, out)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if len(offsets) != 0 {
		t.Errorf("offsets = %v, want empty", offsets)
	}
}

// Invalid bytes are replaced with U+FFFD; the count and offsets pin
// down where the bad bytes were so a developer can locate the source.
func TestSanitizeUTF8_InvalidBytesReplaced(t *testing.T) {
	// Three invalid bytes embedded in otherwise-valid text.
	in := "good\xfftext\xfemore\xfd"
	out, count, offsets := sanitizeUTF8(in)
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	wantOffsets := []int{4, 9, 14}
	if len(offsets) != len(wantOffsets) {
		t.Fatalf("offsets len = %d, want %d (%v)", len(offsets), len(wantOffsets), offsets)
	}
	for i := range wantOffsets {
		if offsets[i] != wantOffsets[i] {
			t.Errorf("offset[%d] = %d, want %d", i, offsets[i], wantOffsets[i])
		}
	}
	if strings.Contains(out, "\xff") || strings.Contains(out, "\xfe") || strings.Contains(out, "\xfd") {
		t.Errorf("invalid bytes survived sanitization: %q", out)
	}
	if !strings.Contains(out, "good") || !strings.Contains(out, "text") {
		t.Errorf("surrounding valid text was lost: %q", out)
	}
}

// Offset capture caps at 10 entries so a prompt that is mostly garbage
// cannot bloat the result struct. The count still reflects every
// replacement made.
func TestSanitizeUTF8_OffsetsCappedTotalNot(t *testing.T) {
	in := strings.Repeat("\xff", 50)
	_, count, offsets := sanitizeUTF8(in)
	if count != 50 {
		t.Errorf("count = %d, want 50", count)
	}
	if len(offsets) != 10 {
		t.Errorf("offsets len = %d, want 10 (cap)", len(offsets))
	}
}

// The registry sanitizes the prompt once after redaction and reports
// the stats on the result. The downstream writer sees the cleaned
// prompt - never the original invalid bytes.
func TestGenerateText_SanitizesPromptAndReportsStats(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: "ok"}
	r := NewWriterRegistry(cap)

	in := "before\xffafter"
	got, err := r.GenerateText(context.Background(), in)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if strings.Contains(cap.captured, "\xff") {
		t.Errorf("invalid byte leaked to writer: %q", cap.captured)
	}
	if got.PromptBadByteCount != 1 {
		t.Errorf("PromptBadByteCount = %d, want 1", got.PromptBadByteCount)
	}
	if len(got.PromptBadByteOffsets) != 1 || got.PromptBadByteOffsets[0] != 6 {
		t.Errorf("PromptBadByteOffsets = %v, want [6]", got.PromptBadByteOffsets)
	}
}

// Per-writer timeouts cap how long one slow writer can block the
// chain. When the writer's Generate blocks past the configured cap,
// the registry's ctx.WithTimeout fires and the writer reports a
// context-deadline error; the chain falls through to the next writer
// without waiting the global 120s.
func TestGenerateText_PerWriterTimeoutCapsSlowWriter(t *testing.T) {
	slow := &slowWriter{name: "claude_code", delay: time.Second}
	fast := &captureWriter{name: "codex", model: "test", response: "ok"}
	r := NewWriterRegistryWithTimeouts(
		[]Writer{slow, fast},
		map[string]time.Duration{"claude_code": 50 * time.Millisecond},
	)

	start := time.Now()
	got, err := r.GenerateText(context.Background(), "hello")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if got.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", got.Provider)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("chain blocked for %v; per-writer timeout should have capped slow at ~50ms", elapsed)
	}
}

// uninstalledWriter mimics a provider whose CLI is not on the host.
// Find() returning empty must short-circuit the registry before any
// subprocess attempt: Generate must never be called, and the per-
// writer timeout (if any) must not start a clock against thin air.
type uninstalledWriter struct {
	name        string
	t           *testing.T
	generateHit bool
}

func (u *uninstalledWriter) Name() string  { return u.name }
func (u *uninstalledWriter) Model() string { return "test" }
func (u *uninstalledWriter) Find() string  { return "" }
func (u *uninstalledWriter) Generate(_ context.Context, _, _ string) (string, error) {
	u.generateHit = true
	u.t.Errorf("Generate must not be called on an uninstalled writer")
	return "", errors.New("must not be called")
}

// Writers whose CLI is not present are skipped without spending wall
// time: Generate is never invoked, and the chain advances to the next
// writer immediately. Cheap pre-flight detection is what keeps an
// uninstalled provider from contributing to total run time.
func TestGenerateText_SkipsUninstalledWriters(t *testing.T) {
	missing := &uninstalledWriter{name: "claude_code", t: t}
	installed := &captureWriter{name: "codex", model: "test", response: "ok"}
	r := NewWriterRegistryWithTimeouts(
		[]Writer{missing, installed},
		map[string]time.Duration{"claude_code": time.Hour},
	)

	got, err := r.GenerateText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if got.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", got.Provider)
	}
	if missing.generateHit {
		t.Errorf("Generate was invoked on an uninstalled writer")
	}
	if len(got.FallbackErrors) != 0 {
		t.Errorf("FallbackErrors = %v; uninstalled writers must not surface as failures", got.FallbackErrors)
	}
}

// Failures encountered before the winning writer are surfaced on the
// result so the activity log can name which providers wasted wall time.
func TestGenerateText_PopulatesFallbackErrorsOnSuccess(t *testing.T) {
	bad := &failingWriter{name: "claude_code", err: errors.New("timed out after 2m0s")}
	good := &captureWriter{name: "copilot", model: "test", response: "ok"}
	r := NewWriterRegistry(bad, good)

	got, err := r.GenerateText(context.Background(), "hello")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if got.Provider != "copilot" {
		t.Errorf("Provider = %q, want copilot", got.Provider)
	}
	if len(got.FallbackErrors) != 1 {
		t.Fatalf("FallbackErrors len = %d, want 1", len(got.FallbackErrors))
	}
	if !strings.Contains(got.FallbackErrors[0], "claude_code") ||
		!strings.Contains(got.FallbackErrors[0], "timed out") {
		t.Errorf("FallbackErrors[0] = %q, want claude_code + timed out", got.FallbackErrors[0])
	}
}
