package intentgap

import "testing"

// A bundle with no actions produces an empty ledger that is still
// safe to index without nil-checks downstream.
func TestBuildTrajectoryLedger_EmptyBundleIsSafe(t *testing.T) {
	got := BuildTrajectoryLedger(Bundle{})
	if got.ByActionID == nil {
		t.Errorf("ByActionID must be non-nil for safe indexing")
	}
	if len(got.All) != 0 {
		t.Errorf("All should be empty; got %v", got.All)
	}
}

// When the detector emits a candidate, every ActionID in that
// candidate becomes a ByActionID entry pointing at the same struct
// in All. This is what callers rely on to ask "is this action part
// of any detected trajectory?" without re-running detection.
func TestBuildTrajectoryLedger_IndexesEveryActionInACandidate(t *testing.T) {
	bundle := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "x.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "x.go", LineRangeStart: 15, LineRangeEnd: 25},
		},
	}
	got := BuildTrajectoryLedger(bundle)
	if len(got.All) != 1 {
		t.Fatalf("All len = %d, want 1", len(got.All))
	}
	if got.ByActionID["a1"] == nil || got.ByActionID["a2"] == nil {
		t.Fatalf("ByActionID missing entries: %+v", got.ByActionID)
	}
	if got.ByActionID["a1"] != got.ByActionID["a2"] {
		t.Errorf("ByActionID entries from the same candidate must share a pointer")
	}
	// Both pointers should resolve to the same data the All slice
	// exposes — same File, same line range.
	if got.ByActionID["a1"].File != got.All[0].File {
		t.Errorf("ByActionID does not point into All; got %+v vs %+v", got.ByActionID["a1"], got.All[0])
	}
}

// Actions outside any detected trajectory don't appear in ByActionID.
// This keeps "is this action in a trajectory" answerable as a plain
// presence check.
func TestBuildTrajectoryLedger_OnlyMembersIndexed(t *testing.T) {
	bundle := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a_traj_1", FilePath: "x.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a_traj_2", FilePath: "x.go", LineRangeStart: 15, LineRangeEnd: 25},
			{ActionID: "a_lonely", FilePath: "y.go", LineRangeStart: 10, LineRangeEnd: 20},
		},
	}
	got := BuildTrajectoryLedger(bundle)
	if _, ok := got.ByActionID["a_traj_1"]; !ok {
		t.Errorf("trajectory member missing from ByActionID")
	}
	if _, ok := got.ByActionID["a_lonely"]; ok {
		t.Errorf("lone action incorrectly indexed: %v", got.ByActionID["a_lonely"])
	}
}
