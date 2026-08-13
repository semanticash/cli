package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hexEventID(seed string) string { return strings.Repeat(seed, 32) }

func partialRec(eventID string, ts int64) PendingPartialRecord {
	return PendingPartialRecord{
		Key: key("tu-pp"), EventID: eventID, Reason: "pre_snapshot_missing",
		ToolName: "Bash", CommandSummary: "cmd", Timestamp: ts,
	}
}

func TestPendingPartialFirstRecordWins(t *testing.T) {
	r := testRegistry(t)
	id := hexEventID("ab")
	first, err := r.LoadOrRecordPendingPartial(partialRec(id, 5000))
	if err != nil || first.Timestamp != 5000 {
		t.Fatalf("first = %+v err = %v", first, err)
	}
	second, err := r.LoadOrRecordPendingPartial(partialRec(id, 9000))
	if err != nil {
		t.Fatal(err)
	}
	if second.Timestamp != 5000 || second.CommandSummary != "cmd" {
		t.Fatalf("second = %+v, want the first record's fields", second)
	}

	recs, err := r.PendingPartialRecords()
	if err != nil || len(recs) != 1 || recs[0].EventID != id {
		t.Fatalf("records = %+v err = %v", recs, err)
	}
}

func TestPendingPartialMalformedFailsClosed(t *testing.T) {
	r := testRegistry(t)
	id := hexEventID("ad")
	if err := os.WriteFile(filepath.Join(r.partialsDir(), id), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadOrRecordPendingPartial(partialRec(id, 5000)); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("load over garbage: err = %v, want corrupt", err)
	}
	if _, err := r.PendingPartialRecords(); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("list with garbage: err = %v, want corrupt", err)
	}
}

func TestPendingPartialRejectsInvalidRecords(t *testing.T) {
	r := testRegistry(t)
	for _, id := range []string{
		"", ".", "..", "a/b", `a\b`, "CON", "evt-1", ".tmp-x",
		strings.Repeat("ab", 16),          // too short
		strings.Repeat("AB", 32),          // uppercase
		strings.Repeat("ab", 31) + "zz",   // non-hex
		strings.Repeat("ab", 32) + "abcd", // too long
	} {
		if _, err := r.LoadOrRecordPendingPartial(partialRec(id, 5000)); err == nil {
			t.Errorf("event id %q accepted", id)
		}
	}
	missingTool := partialRec(hexEventID("ab"), 5000)
	missingTool.ToolName = ""
	if _, err := r.LoadOrRecordPendingPartial(missingTool); err == nil {
		t.Error("record without tool name accepted")
	}
}

// Maintenance preserves pending partial records for recovery.
func TestMaintainKeepsPartialRecords(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	id := hexEventID("0a")
	if _, err := reg.LoadOrRecordPendingPartial(partialRec(id, 5000)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-DefaultStaleWindowAge - time.Hour)
	if err := os.Chtimes(filepath.Join(reg.partialsDir(), id), old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Maintain(context.Background(), reg, 0); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.PendingPartialRecords()
	if err != nil || len(recs) != 1 || recs[0].EventID != id {
		t.Fatalf("records after maintenance = %+v err = %v, want the aged record kept", recs, err)
	}
}

// RemovePendingPartial consumes a record and rejects unsafe names.
func TestRemovePendingPartial(t *testing.T) {
	r := testRegistry(t)
	id := hexEventID("0b")
	if _, err := r.LoadOrRecordPendingPartial(partialRec(id, 5000)); err != nil {
		t.Fatal(err)
	}
	if err := r.RemovePendingPartial(id); err != nil {
		t.Fatal(err)
	}
	if recs, err := r.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records after remove = %+v err = %v", recs, err)
	}
	// Removal is idempotent; invalid identities are rejected.
	if err := r.RemovePendingPartial(id); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	if err := r.RemovePendingPartial("../escape"); err == nil {
		t.Fatal("unsafe event id accepted")
	}
}
