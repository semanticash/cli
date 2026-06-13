package intentgap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// stubWriter is the minimal implementation of llm.Writer the tests
// need. binPath is overridden per-case to simulate "installed" vs
// "not installed".
type stubWriter struct {
	name    string
	model   string
	binPath string
}

func (s *stubWriter) Name() string  { return s.name }
func (s *stubWriter) Model() string { return s.model }
func (s *stubWriter) Find() string  { return s.binPath }
func (s *stubWriter) Generate(context.Context, string, string) (string, error) {
	return "", nil
}

// First writer with a non-empty Find() wins. Order matches the
// registry's fallback chain so the recorded provider matches what
// analysis would have used.
func TestPickInstalledProvider_FirstInstalledWins(t *testing.T) {
	reg := llm.NewWriterRegistry(
		&stubWriter{name: "claude_code", binPath: ""}, // not installed
		&stubWriter{name: "codex", model: "gpt-5", binPath: "/usr/bin/codex"},
		&stubWriter{name: "cursor", binPath: "/usr/bin/cursor"},
	)
	got, err := PickInstalledProvider(reg)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.Name != "codex" {
		t.Errorf("Name = %q, want codex", got.Name)
	}
	if got.Model != "gpt-5" {
		t.Errorf("Model = %q, want gpt-5", got.Model)
	}
}

// Writers whose local Name() diverges from the wire enum (e.g. cursor
// the writer vs cursor_cli the API enum) must be returned under the
// wire enum so the upload request validates for users whose first
// installed CLI is Cursor or Copilot.
func TestPickInstalledProvider_TranslatesToWireEnum(t *testing.T) {
	cases := []struct {
		writerName string
		wantWire   string
	}{
		{"cursor", "cursor_cli"},
		{"copilot", "copilot_cli"},
	}
	for _, tc := range cases {
		t.Run(tc.writerName, func(t *testing.T) {
			reg := llm.NewWriterRegistry(&stubWriter{name: tc.writerName, binPath: "/fake/bin"})
			got, err := PickInstalledProvider(reg)
			if err != nil {
				t.Fatalf("pick: %v", err)
			}
			if got.Name != tc.wantWire {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantWire)
			}
		})
	}
}

// Every production writer registered in providers.NewWriterRegistry
// must have a wire mapping. This catches the case where a new writer
// lands without the table entry it needs, so uploads from machines
// where that writer is the first installed CLI would otherwise be
// rejected at the API.
func TestPickInstalledProvider_AllProductionWritersHaveWireMapping(t *testing.T) {
	productionWriters := []llm.Writer{
		llm.Claude(),
		llm.Codex(),
		llm.Cursor(),
		llm.Gemini(),
		llm.Copilot(),
		llm.KiroCLI(),
	}
	for _, w := range productionWriters {
		t.Run(w.Name(), func(t *testing.T) {
			if _, ok := MapWriterNameToWire(w.Name()); !ok {
				t.Errorf("writer name %q has no wire mapping; add to writerNameToWireProvider", w.Name())
			}
		})
	}
}

// An installed writer with no wire mapping is skipped rather than
// uploaded under a guessed name. The error reports the unmapped
// writer so the developer knows a new entry is needed in the table.
func TestPickInstalledProvider_UnmappedWriterIsSkipped(t *testing.T) {
	reg := llm.NewWriterRegistry(
		&stubWriter{name: "experimental_writer", binPath: "/fake/bin"},
	)
	_, err := PickInstalledProvider(reg)
	if err == nil {
		t.Fatalf("expected error for unmapped writer")
	}
	if !errors.Is(err, ErrNoInstalledProvider) {
		t.Errorf("err should wrap ErrNoInstalledProvider; got %v", err)
	}
	if !strings.Contains(err.Error(), "experimental_writer") {
		t.Errorf("err should name the unmapped writer; got %v", err)
	}
}

// When no writer is installed, the picker returns ErrNoInstalledProvider
// so the caller can treat it as a skip.
func TestPickInstalledProvider_NoneInstalled(t *testing.T) {
	reg := llm.NewWriterRegistry(
		&stubWriter{name: "claude_code", binPath: ""},
		&stubWriter{name: "codex", binPath: ""},
	)
	_, err := PickInstalledProvider(reg)
	if !errors.Is(err, ErrNoInstalledProvider) {
		t.Fatalf("err = %v, want ErrNoInstalledProvider", err)
	}
}

// A nil registry is treated as "none installed" rather than panicking.
// Useful for tests that don't bother wiring a registry, and as a
// defense against accidental nil from a misconfigured caller.
func TestPickInstalledProvider_NilRegistry(t *testing.T) {
	_, err := PickInstalledProvider(nil)
	if !errors.Is(err, ErrNoInstalledProvider) {
		t.Fatalf("err = %v, want ErrNoInstalledProvider", err)
	}
}
