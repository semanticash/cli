package llm

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/semanticash/cli/internal/redact"
)

func sampleSlackWebhook() string {
	return "https://hooks.slack.com/" +
		"services/" +
		"T01234567/" +
		"B01234567/" +
		"xyzXYZ1234567890abcdefgh"
}

// captureWriter records the prompt it receives and returns a fixed
// response. Find() returns a non-empty path so the registry treats
// the writer as installed without ever invoking a real binary.
type captureWriter struct {
	name     string
	model    string
	response string
	captured string
}

func (c *captureWriter) Name() string  { return c.name }
func (c *captureWriter) Model() string { return c.model }
func (c *captureWriter) Find() string  { return "/usr/bin/true" }
func (c *captureWriter) Generate(_ context.Context, _, prompt string) (string, error) {
	c.captured = prompt
	return c.response, nil
}

// stubResult is the canned narrative JSON used by Generate-shaped
// tests so parseNarrativeJSON succeeds without a real LLM.
const stubResult = `{"title":"test","intent":"test","outcome":"test","learnings":[],"friction":[],"open_items":[],"keywords":[]}`

func TestGenerateText_RedactsSecretInPrompt(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: "mock response"}
	r := NewWriterRegistry(cap)

	prompt := "Analyze this diff:\n+slack = " + sampleSlackWebhook() + "\n+normal code here"

	if _, err := r.GenerateText(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(cap.captured, "hooks.slack.com/services/T01234567") {
		t.Error("secret was not redacted before reaching writer")
	}
	if !strings.Contains(cap.captured, "[REDACTED]") {
		t.Error("expected [REDACTED] in prompt sent to writer")
	}
	if !strings.Contains(cap.captured, "normal code here") {
		t.Error("non-secret content was incorrectly removed")
	}
}

func TestGenerateText_SafePromptUnchanged(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: "mock response"}
	r := NewWriterRegistry(cap)

	prompt := "Analyze this diff:\n+func main() { fmt.Println(\"hello\") }\n"

	if _, err := r.GenerateText(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}

	if cap.captured != prompt {
		t.Errorf("safe prompt was modified:\n  got:  %q\n  want: %q", cap.captured, prompt)
	}
}

func TestGenerate_RedactsSecretInPrompt(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: stubResult}
	r := NewWriterRegistry(cap)

	prompt := "Summarize:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD\n-----END RSA PRIVATE KEY-----\n"

	if _, err := r.Generate(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(cap.captured, "MIIEpAIBAAK") {
		t.Error("private key was not redacted before reaching writer in Generate()")
	}
	if !strings.Contains(cap.captured, "[REDACTED]") {
		t.Error("expected [REDACTED] in prompt sent to writer")
	}
}

func TestGenerateText_FailClosed_OnDetectorInitError(t *testing.T) {
	cleanup := redact.ForceInitError(fmt.Errorf("forced detector failure"))
	defer cleanup()

	r := NewWriterRegistry(&errorOnCallWriter{t: t, name: "test"})

	_, err := r.GenerateText(context.Background(), "any prompt content")
	if err == nil {
		t.Fatal("expected error when redactor init fails")
	}
	if !strings.Contains(err.Error(), "egress redaction failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestGenerate_FailClosed_OnDetectorInitError(t *testing.T) {
	cleanup := redact.ForceInitError(fmt.Errorf("forced detector failure"))
	defer cleanup()

	r := NewWriterRegistry(&errorOnCallWriter{t: t, name: "test"})

	_, err := r.Generate(context.Background(), "any prompt content")
	if err == nil {
		t.Fatal("expected error when redactor init fails")
	}
	if !strings.Contains(err.Error(), "egress redaction failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- Registry behavior tests ---

// TestWriterRegistry_RedactionRunsOnce drives the redactor-seam
// invariant: redaction runs exactly once before any writer sees the
// prompt, regardless of how many writers exist in the fallback
// chain. The first writer errors so the registry falls through to
// the second; both writers must receive the same redacted prompt
// and the counting redactor must report exactly one call.
func TestWriterRegistry_RedactionRunsOnce(t *testing.T) {
	var calls int32
	countingRedactor := func(p string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "REDACTED:" + p, nil
	}

	first := &alwaysErrorWriter{name: "first"}
	second := &captureWriter{name: "second", model: "m", response: "ok"}

	r := NewWriterRegistry(first, second)
	r.redactor = countingRedactor

	res, err := r.GenerateText(context.Background(), "original prompt")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("redactor invoked %d times, want exactly 1", got)
	}
	want := "REDACTED:original prompt"
	if second.captured != want {
		t.Errorf("second writer received %q, want %q", second.captured, want)
	}
	if first.calledWith != "" && first.calledWith != want {
		t.Errorf("first writer (errored) received %q, want %q", first.calledWith, want)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want %q", res.Text, "ok")
	}
}

// TestWriterRegistry_UnavailableWritersSkipped ensures a writer
// whose Find() returns empty is never asked to Generate. The
// registry must proceed to the next writer without invoking the
// unavailable one's subprocess.
func TestWriterRegistry_UnavailableWritersSkipped(t *testing.T) {
	uninstalled := &unavailableWriter{name: "uninstalled", t: t}
	installed := &captureWriter{name: "installed", model: "m", response: "ok"}

	r := NewWriterRegistry(uninstalled, installed)

	res, err := r.GenerateText(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if res.Provider != "installed" {
		t.Errorf("Provider = %q, want installed", res.Provider)
	}
}

// TestWriterRegistry_FallbackSuccess covers the recovery path:
// writer 1 returns an error, writer 2 succeeds. The registry
// returns writer 2's result with no error surfaced to the caller.
// the contract is "best-effort fallback", and a hidden upstream
// failure is acceptable when a later writer recovers.
func TestWriterRegistry_FallbackSuccess(t *testing.T) {
	first := &alwaysErrorWriter{name: "first"}
	second := &captureWriter{name: "second", model: "m", response: "recovered"}

	r := NewWriterRegistry(first, second)

	res, err := r.GenerateText(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "recovered" || res.Provider != "second" {
		t.Errorf("got Text=%q Provider=%q, want Text=recovered Provider=second", res.Text, res.Provider)
	}
}

// TestWriterRegistry_AllFailChainedError covers the terminal-error
// path: every writer fails. The returned error must name each
// failing writer and include each writer's error text in fallback
// order so log readers can diagnose multi-writer failures without
// re-running.
func TestWriterRegistry_AllFailChainedError(t *testing.T) {
	first := &alwaysErrorWriter{name: "first"}
	second := &alwaysErrorWriter{name: "second"}

	r := NewWriterRegistry(first, second)

	_, err := r.GenerateText(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error when every writer fails")
	}
	msg := err.Error()
	for _, want := range []string{"first", "second", "all providers failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("chained error missing %q: %v", want, msg)
		}
	}
}

// TestWriterRegistry_NotInstalledMessageEnumeratesAllWriters guards
// the "no AI CLI found" install hint. The message is built from
// registry.List() so new writers auto-include without manual edits.
// The production registry (Claude / Codex / Cursor / Gemini /
// Copilot / Kiro CLI) must produce a message that names every
// installable CLI.
func TestWriterRegistry_NotInstalledMessageEnumeratesAllWriters(t *testing.T) {
	none := &unavailableWriter{name: "claude_code", t: t}
	noneCodex := &unavailableWriter{name: "codex", t: t}
	noneCursor := &unavailableWriter{name: "cursor", t: t}

	r := NewWriterRegistry(none, noneCodex, noneCursor)

	_, err := r.GenerateText(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error when no writer is installed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no AI CLI found") {
		t.Errorf("missing 'no AI CLI found' prefix: %v", err)
	}
	for _, want := range []string{"Claude Code", "Codex", "Cursor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("install hint missing %q: %v", want, msg)
		}
	}
}

// --- Test-only Writer helpers ---

// alwaysErrorWriter reports as installed but always returns an
// error from Generate. Records the prompt it received for the
// redaction-runs-once assertion.
type alwaysErrorWriter struct {
	name       string
	calledWith string
}

func (w *alwaysErrorWriter) Name() string  { return w.name }
func (w *alwaysErrorWriter) Model() string { return "stub" }
func (w *alwaysErrorWriter) Find() string  { return "/usr/bin/true" }
func (w *alwaysErrorWriter) Generate(_ context.Context, _, prompt string) (string, error) {
	w.calledWith = prompt
	return "", fmt.Errorf("%s synthetic failure", w.name)
}

// unavailableWriter reports Find()="" so the registry skips it. If
// Generate is ever called (registry bug), it fails the test.
type unavailableWriter struct {
	name string
	t    *testing.T
}

func (w *unavailableWriter) Name() string  { return w.name }
func (w *unavailableWriter) Model() string { return "stub" }
func (w *unavailableWriter) Find() string  { return "" }
func (w *unavailableWriter) Generate(_ context.Context, _, _ string) (string, error) {
	w.t.Fatalf("Generate() called on unavailable writer %q; registry should have skipped it", w.name)
	return "", nil
}

// errorOnCallWriter is the redaction-failure counterpart: the
// registry should never call Generate when redaction itself fails.
// Used by the FailClosed tests.
type errorOnCallWriter struct {
	name string
	t    *testing.T
}

func (w *errorOnCallWriter) Name() string  { return w.name }
func (w *errorOnCallWriter) Model() string { return "stub" }
func (w *errorOnCallWriter) Find() string  { return "/usr/bin/true" }
func (w *errorOnCallWriter) Generate(_ context.Context, _, _ string) (string, error) {
	w.t.Fatal("writer should not be called when redaction fails")
	return "", nil
}
