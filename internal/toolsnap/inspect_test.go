package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Inspection overlays completion receipts without consuming them.
func TestInspectRegistryOverlaysReceipts(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	// Leave completion recorded only in a receipt.
	if err := r.writeReceipt(ctx, completionReceipt{
		Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 200},
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	groups := snap.CompleteGroups()
	if len(groups) != 1 || len(groups[0].Members) != 1 || groups[0].Members[0].EventID != "evt-1" {
		t.Fatalf("groups = %+v, want the receipt-completed member visible", groups)
	}
	// The receipt file was not consumed.
	entries, err := os.ReadDir(r.receiptsDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("receipts after inspection = %v err = %v, want untouched", entries, err)
	}
}

// Inspection overlays Done receipts without publishing their closure.
func TestInspectRegistryDoneReceiptWritesNothing(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.writeReceipt(ctx, completionReceipt{
		Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 200},
		GroupID: gid, Done: true,
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := InspectRegistry(filepath.Dir(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("windows = %+v, want the done group overlaid away", snap.Windows)
	}
	if entries, err := os.ReadDir(r.closuresDir()); err != nil || len(entries) != 0 {
		t.Fatalf("closure markers after inspection = %v err = %v, want none", entries, err)
	}
	if entries, err := os.ReadDir(r.receiptsDir()); err != nil || len(entries) != 1 {
		t.Fatalf("receipts after inspection = %v err = %v, want untouched", entries, err)
	}

	// A locked pass publishes the closure and consumes the receipt.
	if wins, err := r.Stale(ctx, time.Now().UnixMilli()); err != nil || len(wins) != 0 {
		t.Fatalf("windows after locked pass = %+v err = %v", wins, err)
	}
	if closed, err := r.hasClosureMarker(key("tu1")); err != nil || !closed {
		t.Fatalf("closure marker after locked pass: %v err = %v", closed, err)
	}
	if entries, err := os.ReadDir(r.receiptsDir()); err != nil || len(entries) != 0 {
		t.Fatalf("receipts after locked pass = %v err = %v, want consumed", entries, err)
	}
}

// Inspection enforces the same invariants a locked pass would.
func TestInspectRegistryValidatesState(t *testing.T) {
	r := testRegistry(t)
	bad := `{"windows":[{"key":{"repository_id":"repo-1","provider":"p","session_id":"s","turn_id":"t","tool_use_id":"a"},"group_id":"g-1","status":"bizarre","started_at":1}]}`
	if err := os.WriteFile(r.statePath(), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRegistry(filepath.Dir(r.dir)); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("invalid state inspected: err = %v, want corrupt", err)
	}
}

// A removed tombstoned window returns ErrWindowTombstoned.
func TestCompleteAfterAbandonmentReportsTombstone(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	staleAt := time.Now().Add(-2 * DefaultStaleWindowAge).UnixMilli()
	e := entry("tu1", 100)
	e.StartedAt = staleAt
	if _, err := r.Begin(ctx, e); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := r.ReclaimSealedGroups(ctx, time.Now().UnixMilli())
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaimed = %+v err = %v", reclaimed, err)
	}

	// Complete after abandonment.
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "evt-1", At: 300}, nil, noFinalize(t)); !errors.Is(err, ErrWindowTombstoned) {
		t.Fatalf("err = %v, want ErrWindowTombstoned", err)
	}
}

// A tombstoned identity cannot open a new window.
func TestCaptureAndBeginRefusesTombstonedKey(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	r, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := r.WriteTombstone(key("tu1"), 100); err != nil {
		t.Fatal(err)
	}

	win, err := r.CaptureAndBegin(ctx, s, key("tu1"), "Bash", 200)
	if err != nil || win.Key.ToolUseID != "" {
		t.Fatalf("win = %+v err = %v, want a silent no-op", win, err)
	}
	if wins, err := r.Stale(ctx, time.Now().UnixMilli()); err != nil || len(wins) != 0 {
		t.Fatalf("windows = %+v err = %v, want none", wins, err)
	}
	if refs, err := s.ListRefs(ctx); err != nil || len(refs) != 0 {
		t.Fatalf("refs = %v err = %v, want no snapshot captured", refs, err)
	}
}

// Reclamation preserves a group completed by a receipt.
func TestReclaimSealedGroupsSparesReceiptCompleted(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	staleAt := time.Now().Add(-2 * DefaultStaleWindowAge).UnixMilli()
	completedKey, abandonedKey := key("tu-done"), key("tu-gone")
	doneEntry := entry("tu-done", 100)
	doneEntry.StartedAt = staleAt
	if _, err := r.Begin(ctx, doneEntry); err != nil {
		t.Fatal(err)
	}
	// Record completion only in a receipt.
	if err := r.writeReceipt(ctx, completionReceipt{
		Key: completedKey, Info: CompletionInfo{EventID: "evt-done", At: 200},
	}); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := r.ReclaimSealedGroups(ctx, time.Now().UnixMilli())
	if err != nil || len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %+v err = %v, want the receipt-completed group spared", reclaimed, err)
	}
	pending, err := r.PendingFinalizations(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v err = %v, want the group recoverable", pending, err)
	}

	// A separate expired active group remains reclaimable.
	goneEntry := entry("tu-gone", 100)
	goneEntry.StartedAt = staleAt
	if _, err := r.Begin(ctx, goneEntry); err != nil {
		t.Fatal(err)
	}
	reclaimed, err = r.ReclaimSealedGroups(ctx, time.Now().UnixMilli())
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Tombstoned != 1 {
		t.Fatalf("reclaimed = %+v err = %v, want one stuck group", reclaimed, err)
	}
	if tombstoned, err := r.HasTombstone(abandonedKey); err != nil || !tombstoned {
		t.Fatalf("abandoned member not tombstoned: %v err = %v", tombstoned, err)
	}
	if tombstoned, err := r.HasTombstone(completedKey); err != nil || tombstoned {
		t.Fatalf("spared member tombstoned: %v err = %v", tombstoned, err)
	}
}
