package hooks

import (
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
)

// Ancestry and call mappings require retention even without requests or observations.
func TestObservedInputBatch_MetadataOnlyIsNotEmpty(t *testing.T) {
	if !(ObservedInputBatch{}).empty() {
		t.Fatal("a batch with nothing should be empty")
	}
	ancestryOnly := ObservedInputBatch{Ancestry: map[string]string{"b": "a"}}
	if ancestryOnly.empty() {
		t.Fatal("ancestry-only batch must be retained, not skipped")
	}
	callsOnly := ObservedInputBatch{CallOwners: map[string]string{"tool-1": "rec-1"}}
	if callsOnly.empty() {
		t.Fatal("call-registry-only batch must be retained, not skipped")
	}
	turnsOnly := ObservedInputBatch{Turns: []observedinput.Evidence{{}}}
	if turnsOnly.empty() {
		t.Fatal("turn-bearing batch must be retained")
	}
}
