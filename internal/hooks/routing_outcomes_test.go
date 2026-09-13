package hooks

import (
	"testing"

	"github.com/semanticash/cli/internal/broker"
)

// Separate batches to one repository retain independent event outcomes.
func TestRouteWriteOutcomes_PerEventDestinationNotCollapsed(t *testing.T) {
	o := routeWriteOutcomes{}
	// The first batch persists A.
	o.record("/repos/api", []broker.RawEvent{{EventID: "A"}}, true)
	// A later batch fails to persist B at the same destination.
	o.record("/repos/api", []broker.RawEvent{{EventID: "B"}}, false)

	if !o.persisted("/repos/api", "A") {
		t.Errorf("A must stay persisted despite a later failed batch to the same repo")
	}
	if o.persisted("/repos/api", "B") {
		t.Errorf("B must be not-persisted")
	}
	// Unknown pairs have no recorded write success.
	if o.persisted("/repos/api", "C") || o.persisted("/repos/cli", "A") {
		t.Errorf("unknown pairs must report not-persisted")
	}
}
