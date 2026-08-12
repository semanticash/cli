package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hexEvent builds a valid 64-hex event id from a seed.
func hexEvent(seed byte) string {
	return strings.Repeat(string(rune('a'+seed%6)), 64)
}

// horizon returns the group join window in milliseconds.
func horizon() int64 { return DefaultStaleActiveAge.Milliseconds() }

// Overlapping windows within the join horizon share a group.
func TestOverlapFormsOneGroup(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	gidA, err := r.Begin(ctx, entry("tuA", 1000))
	if err != nil {
		t.Fatal(err)
	}
	// B overlaps A within the horizon.
	gidB, err := r.Begin(ctx, entry("tuB", 1000+horizon()/2))
	if err != nil {
		t.Fatal(err)
	}
	if gidB != gidA {
		t.Fatalf("overlapping windows split: %s vs %s", gidA, gidB)
	}
}

// A fresh member does not extend its group's join horizon.
func TestJoinHorizonSealsGroupDespiteFreshMember(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	// A opens the group.
	gidA, err := r.Begin(ctx, entry("tuA", created))
	if err != nil {
		t.Fatal(err)
	}
	// B joins while A is active.
	mid := created + horizon()*5/6
	gidB, err := r.Begin(ctx, entry("tuB", mid))
	if err != nil {
		t.Fatal(err)
	}
	if gidB != gidA {
		t.Fatalf("B did not join the open group: %s vs %s", gidB, gidA)
	}
	// C arrives after the immutable horizon and starts a new group.
	after := created + horizon() + 1
	gidC, err := r.Begin(ctx, entry("tuC", after))
	if err != nil {
		t.Fatal(err)
	}
	if gidC == gidA {
		t.Fatalf("C joined an expired group; fresh member renewed the horizon")
	}
}

// A new group closes normally while an older group remains sealed.
func TestFreshGroupClosesAfterRotation(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tu-stuck", created)); err != nil {
		t.Fatal(err)
	}
	after := created + horizon() + 1
	freshGID, err := r.Begin(ctx, entry("tu-fresh", after))
	if err != nil {
		t.Fatal(err)
	}

	var members []PendingToolSnapshot
	removed, err := r.Complete(ctx, key("tu-fresh"),
		CompletionInfo{EventID: hexEvent(1), At: after + 1}, nil, doneFinalize(&members))
	if err != nil || !removed {
		t.Fatalf("removed = %v err = %v", removed, err)
	}
	if len(members) != 1 || members[0].Key.ToolUseID != "tu-fresh" {
		t.Fatalf("members = %+v, want the rotated window alone", members)
	}
	if freshGID == "" {
		t.Fatal("fresh group id empty")
	}
}

// A late post seals its group without invoking finalization.
func TestLatePostSealsWithoutFinalizing(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tu-late", created)); err != nil {
		t.Fatal(err)
	}
	// Complete after the join horizon.
	postAt := created + 2*horizon()
	_, err := r.Complete(ctx, key("tu-late"),
		CompletionInfo{EventID: hexEvent(2), At: postAt}, nil, noFinalize(t))
	if !errors.Is(err, ErrWindowSealed) {
		t.Fatalf("err = %v, want ErrWindowSealed", err)
	}
	// Sealed groups are excluded from normal finalization.
	pending, err := r.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want a sealed group excluded from normal finalization", pending)
	}
}

// Reclamation records partials and tombstones every group member.
func TestReclaimSealedGroups(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tu-stuck", created)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, entry("tu-done", created+1)); err != nil {
		t.Fatal(err)
	}
	doneEvent := hexEvent(2)
	// Complete one member while another remains active.
	if _, err := r.Complete(ctx, key("tu-done"),
		CompletionInfo{EventID: doneEvent, At: created + 2, CommandSummary: "make generate"},
		nil, func([]PendingToolSnapshot, *GroupFinal, bool, func() error) (FinalizeResult, error) {
			return FinalizeResult{}, nil // non-final: tu-stuck still active
		}); err != nil {
		t.Fatal(err)
	}

	now := created + 2*horizon()
	reclaimed, err := r.ReclaimSealedGroups(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Completed != 1 || reclaimed[0].Tombstoned != 2 {
		t.Fatalf("reclaimed = %+v, want 1 group, 1 completed, 2 tombstoned", reclaimed)
	}

	recs, err := r.PendingPartialRecords()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v err = %v", recs, err)
	}
	if recs[0].EventID != doneEvent || recs[0].Reason != ReasonStaleActiveWindow {
		t.Fatalf("record = %+v, want %s for the completed member", recs[0], ReasonStaleActiveWindow)
	}
	for _, k := range []ToolKey{key("tu-stuck"), key("tu-done")} {
		if ts, err := r.HasTombstone(k); err != nil || !ts {
			t.Fatalf("tombstone %v missing: %v err = %v", k.ToolUseID, ts, err)
		}
	}

	// A late post remains blocked by its tombstone.
	if _, err := r.Complete(ctx, key("tu-stuck"),
		CompletionInfo{EventID: hexEvent(3), At: now + 5}, nil, noFinalize(t)); !errors.Is(err, ErrWindowTombstoned) {
		t.Fatalf("late post err = %v, want ErrWindowTombstoned", err)
	}

	// Repeated reclamation is a no-op.
	again, err := r.ReclaimSealedGroups(ctx, now)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v err = %v, want no-op", again, err)
	}
	if recs, err := r.PendingPartialRecords(); err != nil || len(recs) != 1 {
		t.Fatalf("records after second pass = %+v err = %v", recs, err)
	}
}

// An open group within its horizon remains registered.
func TestReclaimSparesOpenGroups(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	gid, err := r.Begin(ctx, entry("tu-live", created))
	if err != nil {
		t.Fatal(err)
	}
	// Reclaim before the horizon expires.
	now := created + horizon()/2
	reclaimed, err := r.ReclaimSealedGroups(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %+v, want the open group spared", reclaimed)
	}
	if ts, err := r.HasTombstone(key("tu-live")); err != nil || ts {
		t.Fatalf("open member tombstoned: %v err = %v", ts, err)
	}
	if gid == "" {
		t.Fatal("group id empty")
	}
}

// Reclamation leaves snapshot refs for maintenance.
func TestReclaimLeavesSnapshotRefs(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	r, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.CaptureAndBegin(ctx, s, key("tu-stuck"), "Bash", created); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListRefs(ctx)
	if err != nil || len(before) == 0 {
		t.Fatalf("refs = %v err = %v, want the pre snapshot", before, err)
	}

	now := created + 2*horizon()
	reclaimed, err := r.ReclaimSealedGroups(ctx, now)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaimed = %+v err = %v", reclaimed, err)
	}
	after, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refs = %v, want untouched (%v)", after, before)
	}
}

// A sealed group remains excluded from normal finalization.
func TestSealedGroupNeverFinalizesNormally(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tuA", created)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, entry("tuB", created+1)); err != nil {
		t.Fatal(err)
	}
	// Complete both members after the horizon.
	post := created + 2*horizon()
	if _, err := r.Complete(ctx, key("tuA"),
		CompletionInfo{EventID: hexEvent(1), At: post}, nil, noFinalize(t)); !errors.Is(err, ErrWindowSealed) {
		t.Fatalf("tuA post err = %v, want ErrWindowSealed", err)
	}
	if _, err := r.Complete(ctx, key("tuB"),
		CompletionInfo{EventID: hexEvent(2), At: post + 1}, nil, noFinalize(t)); !errors.Is(err, ErrWindowSealed) {
		t.Fatalf("tuB post err = %v, want ErrWindowSealed even as the last member", err)
	}

	// Pending-finalization queries exclude the group.
	pending, err := r.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the sealed group hidden from finalization", pending)
	}
	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.CompleteGroups()) != 0 {
		t.Fatalf("complete groups = %+v, want the sealed group excluded", snap.CompleteGroups())
	}
	// Explicit resume also rejects the group.
	var gid string
	for id := range snap.Groups {
		gid = id
	}
	if _, err := r.ResumeFinalization(ctx, gid,
		func([]PendingToolSnapshot, *GroupFinal, bool, func() error) (FinalizeResult, error) {
			t.Fatal("finalize invoked for a sealed group")
			return FinalizeResult{}, nil
		}); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("resume err = %v, want a sealed refusal", err)
	}

	// Reclamation converts both completions to partial evidence.
	reclaimed, err := r.ReclaimSealedGroups(ctx, post+2)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Completed != 2 {
		t.Fatalf("reclaimed = %+v err = %v, want both members as partial evidence", reclaimed, err)
	}
}

// A completion racing group removal defers reclamation.
func TestReclaimDefersMemberCompletedMidPass(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tu-race", created)); err != nil {
		t.Fatal(err)
	}
	now := created + 2*horizon()
	raceEvent := hexEvent(7)

	// Complete the active member after recovery files are written.
	fired := false
	r.afterRecoveryWrites = func() {
		fired = true
		if _, err := r.Complete(ctx, key("tu-race"),
			CompletionInfo{EventID: raceEvent, At: now}, func(PendingToolSnapshot) error { return nil },
			noFinalize(t)); !errors.Is(err, ErrWindowSealed) {
			t.Fatalf("mid-pass completion err = %v, want ErrWindowSealed", err)
		}
	}
	reclaimed, err := r.ReclaimSealedGroups(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the race seam did not fire")
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %+v, want the raced group deferred", reclaimed)
	}
	// The first pass did not observe the completion.
	if recs, err := r.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records = %+v err = %v, want none until the next pass", recs, err)
	}

	// The next pass records the completion as partial evidence.
	r.afterRecoveryWrites = nil
	reclaimed, err = r.ReclaimSealedGroups(ctx, now+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Completed != 1 {
		t.Fatalf("reclaimed = %+v, want the completed member as partial evidence", reclaimed)
	}
	recs, err := r.PendingPartialRecords()
	if err != nil || len(recs) != 1 || recs[0].EventID != raceEvent || recs[0].Reason != ReasonStaleActiveWindow {
		t.Fatalf("records = %+v err = %v, want the raced event's partial", recs, err)
	}
}

// Cancellation leaves the sealed group registered for retry.
func TestReclaimHonorsCancellationDuringWrites(t *testing.T) {
	r := testRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created := int64(1000)
	if _, err := r.Begin(context.Background(), entry("tu1", created)); err != nil {
		t.Fatal(err)
	}
	// Cancel before recovery files are written.
	r.beforeRecoveryWrites = cancel

	reclaimed, err := r.ReclaimSealedGroups(ctx, created+2*horizon())
	if err == nil {
		t.Fatal("cancelled reclamation returned no error")
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %+v, want none on cancellation", reclaimed)
	}
	// No recovery files were written.
	if recs, err := r.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records = %+v err = %v, want none", recs, err)
	}
	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || !snap.Groups[snap.Windows[0].GroupID].Sealed {
		t.Fatalf("windows = %+v groups = %+v, want the group retained and sealed", snap.Windows, snap.Groups)
	}
}

// Legacy state derives group metadata from its earliest member.
func TestLegacyStateSynthesizesGroupMeta(t *testing.T) {
	r := testRegistry(t)
	legacy := `{"windows":[` +
		`{"key":{"repository_id":"repo-1","provider":"claude_code","session_id":"s","turn_id":"t","tool_use_id":"a"},` +
		`"tool_name":"Bash","snapshot_ref":"refs/x","tree_hash":"th","head_hash":"hh","object_format":"sha1",` +
		`"started_at":5000,"seq":0,"group_id":"g-legacy","status":"active"},` +
		`{"key":{"repository_id":"repo-1","provider":"claude_code","session_id":"s","turn_id":"t","tool_use_id":"b"},` +
		`"tool_name":"Bash","snapshot_ref":"refs/y","tree_hash":"th","head_hash":"hh","object_format":"sha1",` +
		`"started_at":9000,"seq":1,"group_id":"g-legacy","status":"active"}],"next_seq":2}`
	if err := os.WriteFile(r.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatalf("legacy state rejected: %v", err)
	}
	meta, ok := snap.Groups["g-legacy"]
	if !ok {
		t.Fatalf("groups = %+v, want synthesized metadata", snap.Groups)
	}
	// The earliest member anchors the horizon.
	if meta.CreatedAt != 5000 || meta.JoinUntil != 5000+horizon() {
		t.Fatalf("meta = %+v, want CreatedAt 5000 and an earliest-member horizon", meta)
	}
	if meta.Sealed {
		t.Fatal("synthesized metadata pre-sealed; sealing must be a later operation")
	}
}

// A sealed group remains sealed after reopening the registry.
func TestSealIsDurableAcrossReload(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	created := int64(1000)
	if _, err := r.Begin(ctx, entry("tu1", created)); err != nil {
		t.Fatal(err)
	}
	// A later window seals the old group and starts another.
	after := created + horizon() + 1
	gid2, err := r.Begin(ctx, entry("tu2", after))
	if err != nil {
		t.Fatal(err)
	}

	// Reopen and read the persisted seal.
	r2, err := OpenRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	sealed := 0
	var freshGID string
	for gid, meta := range snap.Groups {
		if meta.Sealed {
			sealed++
		} else {
			freshGID = gid
		}
	}
	if sealed != 1 {
		t.Fatalf("sealed groups = %d, want 1", sealed)
	}
	if freshGID != gid2 {
		t.Fatalf("open group = %s, want the rotated %s", freshGID, gid2)
	}
	_ = r2
}
