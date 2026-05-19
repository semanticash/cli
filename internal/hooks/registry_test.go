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

// unavailableFakeProvider mirrors fakeProvider but reports
// IsAvailable() == false. Used by TestRegistry_ListAvailable to
// verify the filter; fakeProvider's IsAvailable() returns true
// unconditionally and would not exercise the filter path.
type unavailableFakeProvider struct{ fakeProvider }

func (u *unavailableFakeProvider) IsAvailable() bool { return false }

// TestNewRegistry_StoresAllProviders confirms the constructor
// indexes every provider it receives, so Get hits and List length
// match the input set. Argument order in the constructor does not
// have to equal List output order (List enforces canonical order);
// this test only verifies that no provider is dropped during
// construction.
func TestNewRegistry_StoresAllProviders(t *testing.T) {
	r := NewRegistry(
		&fakeProvider{name: "claude-code"},
		&fakeProvider{name: "codex"},
		&fakeProvider{name: "cursor"},
	)
	for _, name := range []string{"claude-code", "codex", "cursor"} {
		if r.Get(name) == nil {
			t.Errorf("Get(%q) = nil; expected provider", name)
		}
	}
	if got := len(r.List()); got != 3 {
		t.Errorf("len(List()) = %d, want 3", got)
	}
}

// TestRegistry_Get_HitAndMiss covers the two outcomes Get is
// documented to produce: a registered name returns the provider,
// an unregistered name returns nil. Hook payloads can carry
// provider names this binary wasn't built with, so the miss path
// is part of the contract and worth pinning.
func TestRegistry_Get_HitAndMiss(t *testing.T) {
	r := NewRegistry(&fakeProvider{name: "claude-code"})
	if got := r.Get("claude-code"); got == nil {
		t.Error("Get(\"claude-code\") = nil; expected provider")
	}
	if got := r.Get("nonexistent"); got != nil {
		t.Errorf("Get(\"nonexistent\") = %v; expected nil for unregistered name", got)
	}
}

// TestRegistry_DuplicateNames_LastWins documents the map-backed
// behavior for duplicate names: the later provider replaces the
// earlier one.
func TestRegistry_DuplicateNames_LastWins(t *testing.T) {
	first := &fakeProvider{name: "claude-code"}
	second := &fakeProvider{name: "claude-code"}
	r := NewRegistry(first, second)
	if got := r.Get("claude-code"); got != second {
		t.Errorf("Get returned %v; want second registration to win", got)
	}
}

// TestRegistry_List_CanonicalOrder confirms that List() always
// returns providers in the canonical order defined by
// providerOrder, regardless of NewRegistry argument order. This
// is the property every consumer relies on: blame output,
// `agents` listings, health-check iteration, and the doctor
// report all assume the same ordering across calls.
func TestRegistry_List_CanonicalOrder(t *testing.T) {
	// Reversed input order; List output must still match
	// providerOrder.
	r := NewRegistry(
		&fakeProvider{name: "kiro-ide"},
		&fakeProvider{name: "kiro-cli"},
		&fakeProvider{name: "gemini-cli"},
		&fakeProvider{name: "copilot"},
		&fakeProvider{name: "cursor"},
		&fakeProvider{name: "codex"},
		&fakeProvider{name: "claude-code"},
	)
	want := []string{
		"claude-code", "codex", "cursor", "copilot",
		"gemini-cli", "kiro-cli", "kiro-ide",
	}
	got := names(r.List())
	for i, name := range want {
		if i >= len(got) || got[i] != name {
			t.Errorf("position %d: got %q, want %q (full: %v)", i, got[i], name, got)
		}
	}
}

// TestRegistry_ListAvailable_FiltersAndPreservesOrder covers the
// IsAvailable() filter used by `semantica agents` and the health
// checks. Available providers must come back in canonical order;
// unavailable providers must not appear at all.
func TestRegistry_ListAvailable_FiltersAndPreservesOrder(t *testing.T) {
	r := NewRegistry(
		&fakeProvider{name: "claude-code"},                                  // available
		&unavailableFakeProvider{fakeProvider: fakeProvider{name: "codex"}}, // not available
		&fakeProvider{name: "cursor"},                                       // available
	)
	got := names(r.ListAvailable())
	want := []string{"claude-code", "cursor"}
	if len(got) != len(want) {
		t.Fatalf("ListAvailable len = %d, want %d (full: %v)", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("position %d: got %q, want %q", i, got[i], name)
		}
	}
}

// TestRegistry_NilReceiver_Safe confirms the test-only nil
// contract: Get behaves like a miss, while List and ListAvailable
// behave like an empty registry.
func TestRegistry_NilReceiver_Safe(t *testing.T) {
	var r *Registry
	if got := r.Get("claude-code"); got != nil {
		t.Errorf("(*Registry)(nil).Get returned %v; want nil", got)
	}
	if got := r.List(); got != nil {
		t.Errorf("(*Registry)(nil).List returned %v; want nil", got)
	}
	if got := r.ListAvailable(); got != nil {
		t.Errorf("(*Registry)(nil).ListAvailable returned %v; want nil", got)
	}
}
