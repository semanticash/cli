package providers

import (
	"reflect"
	"testing"
)

// TestNewHookRegistry_PinsCanonicalProviderSet pins the production
// hook provider set. Commands, services, and health checks all use
// this registry, so dropping or adding a provider here changes
// capture coverage across the binary.
func TestNewHookRegistry_PinsCanonicalProviderSet(t *testing.T) {
	r := NewHookRegistry()

	got := make([]string, 0, 7)
	for _, p := range r.List() {
		got = append(got, p.Name())
	}

	want := []string{
		"claude-code",
		"codex",
		"cursor",
		"copilot",
		"gemini-cli",
		"kiro-cli",
		"kiro-ide",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewHookRegistry().List() names = %v, want %v", got, want)
	}

	// Get must resolve every canonical name, even if List ordering
	// still happens to match.
	for _, name := range want {
		if r.Get(name) == nil {
			t.Errorf("NewHookRegistry().Get(%q) = nil; production wiring lost the provider", name)
		}
	}
}

// TestNewWriterRegistry_PinsFallbackOrder pins the writer fallback
// order. When multiple AI CLIs are installed, the first writer that
// succeeds wins, so reordering changes user-visible behavior.
func TestNewWriterRegistry_PinsFallbackOrder(t *testing.T) {
	r := NewWriterRegistry()

	got := make([]string, 0, 6)
	for _, w := range r.List() {
		got = append(got, w.Name())
	}

	want := []string{
		"claude_code",
		"codex",
		"cursor",
		"gemini_cli",
		"copilot",
		"kiro_cli",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewWriterRegistry().List() names = %v, want %v", got, want)
	}
}
