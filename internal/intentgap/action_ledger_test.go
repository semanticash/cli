package intentgap

import "testing"

// An empty input still yields a non-nil ledger so downstream callers
// can index safely without a nil-check.
func TestBuildActionLedger_EmptyIsSafe(t *testing.T) {
	got := BuildActionLedger(nil)
	if got.ByID == nil || got.ByFile == nil {
		t.Errorf("maps must be non-nil even on empty input: %+v", got)
	}
	if len(got.All) != 0 {
		t.Errorf("All should be empty; got %v", got.All)
	}
}

// All preserves bundle order so downstream consumers (verifier
// prompt, trajectory detection) see actions oldest-first.
func TestBuildActionLedger_PreservesAllOrder(t *testing.T) {
	in := []BundleAgentAction{
		{ActionID: "a1", FilePath: "first.go"},
		{ActionID: "a2", FilePath: "second.go"},
		{ActionID: "a3", FilePath: "third.go"},
	}
	got := BuildActionLedger(in)
	if len(got.All) != 3 || got.All[0].ActionID != "a1" || got.All[2].ActionID != "a3" {
		t.Errorf("order not preserved: %+v", got.All)
	}
}

// ByID indexes every action so cite-or-drop can resolve an
// action_id lookup without scanning the slice.
func TestBuildActionLedger_ByIDIndex(t *testing.T) {
	in := []BundleAgentAction{
		{ActionID: "a1", FilePath: "x.go", ToolName: "Edit"},
		{ActionID: "a2", FilePath: "", ToolName: "Bash"},
	}
	got := BuildActionLedger(in)
	if a, ok := got.ByID["a1"]; !ok || a.ToolName != "Edit" {
		t.Errorf("a1 missing or mangled: %+v", got.ByID)
	}
	if _, ok := got.ByID["a2"]; !ok {
		t.Errorf("a2 missing from ByID; ByID must index every action regardless of FilePath")
	}
}

// ByFile groups action_ids by their FilePath so retrieval can list
// actions per file in O(1). Actions whose FilePath is empty are
// intentionally excluded from this index: there is no file to key on.
func TestBuildActionLedger_ByFileGroupsByPath(t *testing.T) {
	in := []BundleAgentAction{
		{ActionID: "a1", FilePath: "handler.go"},
		{ActionID: "a2", FilePath: "handler.go"},
		{ActionID: "a3", FilePath: ""}, // Bash, no path
		{ActionID: "a4", FilePath: "middleware.go"},
	}
	got := BuildActionLedger(in)
	if len(got.ByFile["handler.go"]) != 2 {
		t.Errorf("handler.go group: %v, want 2 entries", got.ByFile["handler.go"])
	}
	if got.ByFile["handler.go"][0] != "a1" || got.ByFile["handler.go"][1] != "a2" {
		t.Errorf("ByFile must preserve insertion order: %v", got.ByFile["handler.go"])
	}
	if len(got.ByFile["middleware.go"]) != 1 || got.ByFile["middleware.go"][0] != "a4" {
		t.Errorf("middleware.go group: %v", got.ByFile["middleware.go"])
	}
	if _, ok := got.ByFile[""]; ok {
		t.Errorf("ByFile must not index empty paths; got %v", got.ByFile[""])
	}
}

// Mutating the source slice after BuildActionLedger must not change
// the ledger; the ledger owns an independent copy of All.
func TestBuildActionLedger_IsolatedFromSourceMutation(t *testing.T) {
	in := []BundleAgentAction{{ActionID: "a1", FilePath: "x.go"}}
	got := BuildActionLedger(in)
	in[0].ActionID = "MUTATED"
	if got.All[0].ActionID != "a1" {
		t.Errorf("ledger leaked source mutation; All[0]=%q", got.All[0].ActionID)
	}
}
