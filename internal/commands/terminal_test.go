package commands

import (
	"os"
	"testing"
)

func TestIsInteractiveTerminal_DevNullIsFalse(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = f.Close() }()

	if isInteractiveTerminal(f) {
		t.Fatalf("%s should not be treated as an interactive terminal", os.DevNull)
	}
}

func TestIsInteractiveTerminal_PipeIsFalse(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	if isInteractiveTerminal(r) {
		t.Fatal("pipe reader should not be treated as an interactive terminal")
	}
	if isInteractiveTerminal(w) {
		t.Fatal("pipe writer should not be treated as an interactive terminal")
	}
}
