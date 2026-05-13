package hooks

import (
	"testing"
)

// TestSortProviders_CanonicalOrder checks the canonical display
// order for known providers. The "aa-unknown" probe is the
// differentiator: it sorts alphabetically before every
// canonical name, so if any canonical name (e.g. gemini-cli,
// kiro-cli, kiro-ide) is missing from providerOrder it would
// be treated as unknown and the alpha tie-break would place
// aa-unknown ahead of it. With the canonical map intact, every
// canonical provider sorts before any unknown regardless of
// alpha position.
func TestSortProviders_CanonicalOrder(t *testing.T) {
	ps := []HookProvider{
		&fakeProvider{name: "aa-unknown"},
		&fakeProvider{name: "kiro-ide"},
		&fakeProvider{name: "gemini-cli"},
		&fakeProvider{name: "kiro-cli"},
		&fakeProvider{name: "claude-code"},
		&fakeProvider{name: "copilot"},
		&fakeProvider{name: "cursor"},
	}
	sortProviders(ps)
	want := []string{
		"claude-code", "cursor", "copilot",
		"gemini-cli", "kiro-cli", "kiro-ide",
		"aa-unknown",
	}
	for i, p := range ps {
		if p.Name() != want[i] {
			t.Errorf("position %d: got %q, want %q (full order: %v)", i, p.Name(), want[i], names(ps))
		}
	}
}

// TestSortProviders_UnknownsTieBreakByName checks the
// deterministic tie-breaker for providers absent from the
// canonical order map. Without the name tie-breaker, Go's
// non-stable sort.Slice and the randomized map iteration that
// feeds it would produce different orderings across runs.
func TestSortProviders_UnknownsTieBreakByName(t *testing.T) {
	ps := []HookProvider{
		&fakeProvider{name: "zeta-unknown"},
		&fakeProvider{name: "alpha-unknown"},
		&fakeProvider{name: "mu-unknown"},
	}
	for i := 0; i < 5; i++ {
		// Re-shuffle by alternating order; sort must produce
		// the same result every time.
		input := []HookProvider{ps[2], ps[0], ps[1]}
		sortProviders(input)
		want := []string{"alpha-unknown", "mu-unknown", "zeta-unknown"}
		for j, p := range input {
			if p.Name() != want[j] {
				t.Errorf("iteration %d position %d: got %q, want %q", i, j, p.Name(), want[j])
			}
		}
	}
}

// TestSortProviders_KnownsBeforeUnknowns checks that
// every entry in the canonical order map sorts before any
// unknown provider.
func TestSortProviders_KnownsBeforeUnknowns(t *testing.T) {
	ps := []HookProvider{
		&fakeProvider{name: "zeta-unknown"},
		&fakeProvider{name: "kiro-ide"},
		&fakeProvider{name: "alpha-unknown"},
		&fakeProvider{name: "claude-code"},
	}
	sortProviders(ps)
	if ps[0].Name() != "claude-code" {
		t.Errorf("known provider should sort first; got %q", ps[0].Name())
	}
	if ps[1].Name() != "kiro-ide" {
		t.Errorf("known provider should sort before unknowns; got %q", ps[1].Name())
	}
	if ps[2].Name() != "alpha-unknown" {
		t.Errorf("unknowns should sort alphabetically; got %q at position 2", ps[2].Name())
	}
}

func names(ps []HookProvider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}
