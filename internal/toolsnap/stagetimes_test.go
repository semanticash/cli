package toolsnap

import (
	"context"
	"testing"
	"time"
)

func TestStageTimesAccumulate(t *testing.T) {
	_, st := WithStageTimes(context.Background())
	st.record("a", stageLeaf, 10*time.Millisecond)
	st.record("a", stageLeaf, 5*time.Millisecond)
	st.record("x", stageAggregate, 20*time.Millisecond)

	leaf := st.LeafMillis()
	if leaf["a"] != 15 {
		t.Errorf("leaf a = %d ms, want 15", leaf["a"])
	}
	if _, ok := leaf["x"]; ok {
		t.Error("aggregate leaked into leaf map")
	}
	if agg := st.AggregateMillis(); agg["x"] != 20 {
		t.Errorf("agg x = %d ms, want 20", agg["x"])
	}
	if st.LeafTotal() != 15*time.Millisecond {
		t.Errorf("leaf total = %v, want 15ms", st.LeafTotal())
	}
}

// Timing is inert without a collector.
func TestMeasureStageDisabledIsInert(t *testing.T) {
	ctx := context.Background()
	if stageTimesFrom(ctx) != nil {
		t.Fatal("unexpected collector on a bare context")
	}
	stop := measureStage(ctx, "x", stageLeaf)
	stop()
}

// A dirty capture records its leaf and aggregate stages.
func TestCaptureRecordsStages(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	writeFile(t, root, "dirty.txt", "uncommitted change\n")

	ctx, st := WithStageTimes(context.Background())
	if _, err := s.CaptureBefore(ctx); err != nil {
		t.Fatalf("capture: %v", err)
	}

	leaf := st.LeafMillis()
	for _, name := range []string{"resolve_head", "dirty_paths", "hash", "tree_write"} {
		if _, ok := leaf[name]; !ok {
			t.Errorf("missing leaf stage %q; got %v", name, leaf)
		}
	}
	if _, ok := st.AggregateMillis()["capture_before"]; !ok {
		t.Errorf("missing capture_before aggregate; got %v", st.AggregateMillis())
	}
}

// A clean capture skips hashing and tree construction.
func TestCaptureCleanTreeSkipsBuildTree(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)

	ctx, st := WithStageTimes(context.Background())
	if _, err := s.CaptureBefore(ctx); err != nil {
		t.Fatalf("capture: %v", err)
	}
	leaf := st.LeafMillis()
	if _, ok := leaf["dirty_paths"]; !ok {
		t.Errorf("clean capture should still record dirty_paths; got %v", leaf)
	}
	if _, ok := leaf["hash"]; ok {
		t.Errorf("clean capture should not hash; got %v", leaf)
	}
}
