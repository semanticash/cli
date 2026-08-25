package toolsnap

import (
	"context"
	"strings"
	"testing"
)

// Malformed targets must not reach Git or delete refs.
func TestDeleteRefsRejectsMalformedTarget(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	ref := SnapshotRef("main", "g", "tu")
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	victim := SnapshotRef("main", "g", "victim")
	if err := s.CreateRef(ctx, victim, snap.TreeHash); err != nil {
		t.Fatalf("create victim: %v", err)
	}

	bad := []string{
		"",                                     // empty: no compare-and-swap protection
		snap.TreeHash[:10],                     // wrong length
		strings.ToUpper(snap.TreeHash),         // non-lowercase hex
		snap.TreeHash + "\x00delete " + victim, // embedded-NUL injection
		snap.TreeHash + "\nzzzz",               // embedded newline
	}
	for _, target := range bad {
		if err := s.DeleteRefs(ctx, map[string]string{ref: target}); err == nil {
			t.Errorf("DeleteRefs accepted malformed target %q", target)
		}
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if _, ok := refs[ref]; !ok {
		t.Error("target ref deleted despite malformed input")
	}
	if _, ok := refs[victim]; !ok {
		t.Error("victim ref deleted via injected command")
	}
}

// A stale expected target aborts the full batch.
func TestDeleteRefsAtomicAbort(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	// Three distinct trees from three snapshots of evolving worktree state.
	treeA, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("tree A: %v", err)
	}
	writeFile(t, root, "a.txt", "content b\n")
	treeB, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("tree B: %v", err)
	}
	writeFile(t, root, "a.txt", "content c\n")
	treeC, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("tree C: %v", err)
	}
	if treeA.TreeHash == treeB.TreeHash || treeB.TreeHash == treeC.TreeHash || treeA.TreeHash == treeC.TreeHash {
		t.Fatalf("trees not distinct: %s %s %s", treeA.TreeHash, treeB.TreeHash, treeC.TreeHash)
	}

	refA := SnapshotRef("main", "g", "tuA")
	refB := SnapshotRef("main", "g", "tuB")
	refC := SnapshotRef("main", "g", "tuC")
	for ref, tree := range map[string]string{refA: treeA.TreeHash, refB: treeB.TreeHash, refC: treeC.TreeHash} {
		if err := s.CreateRef(ctx, ref, tree); err != nil {
			t.Fatalf("create %s: %v", ref, err)
		}
	}

	// B's expected target is stale (points at A's tree); the batch must abort.
	err = s.DeleteRefs(ctx, map[string]string{
		refA: treeA.TreeHash,
		refB: treeA.TreeHash,
		refC: treeC.TreeHash,
	})
	if err == nil {
		t.Fatal("batch delete with a stale expected target succeeded, want abort")
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	for _, ref := range []string{refA, refB, refC} {
		if _, ok := refs[ref]; !ok {
			t.Errorf("ref %s deleted despite aborted transaction", ref)
		}
	}

	// The matching batch deletes all three atomically.
	if err := s.DeleteRefs(ctx, map[string]string{
		refA: treeA.TreeHash,
		refB: treeB.TreeHash,
		refC: treeC.TreeHash,
	}); err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	refs, err = s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs after delete: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs after batch delete = %v, want none", refs)
	}
}
