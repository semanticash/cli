package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// pendingRecord returns a valid manifest with pending candidates.
func pendingRecord() *ToolWindowObservationRecord {
	return &ToolWindowObservationRecord{
		PrimaryRepoPath:     "/repos/cli",
		PrimaryRepositoryID: "cli-id",
		Candidates: []ObservedCandidate{
			{RepoPath: "/repos/cli", RepositoryID: "cli-id", Outcome: observationPending},
			{RepoPath: "/repos/api", RepositoryID: "api-id", Outcome: observationPending},
		},
		OmittedByCap: 1,
		TotalActive:  3,
	}
}

func TestToolWindowObservation_CreateThenCheckpoint(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	t.Cleanup(func() { toolWindowNow = orig })
	toolWindowNow = func() int64 { return 10_000_000 }

	k := key("codex", "s1", "t1", "call1")
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Distinguish a successful observation from an unattempted snapshot.
	if err := CheckpointToolWindowObservation(k, map[string]string{
		"/repos/cli": observationObserved,
		"/repos/api": observationBudgetExpired,
	}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	got, err := LoadToolWindowObservation(k)
	if err != nil || got == nil {
		t.Fatalf("load: (%+v, %v)", got, err)
	}
	if got.Candidates[0].Outcome != observationObserved || got.Candidates[1].Outcome != observationBudgetExpired {
		t.Fatalf("outcomes = %+v", got.Candidates)
	}
	if got.Provider != "codex" || got.SessionID != "s1" || got.TurnID != "t1" || got.ToolUseID != "call1" {
		t.Fatalf("identity = %+v", got)
	}
}

// Duplicate creation preserves candidates and recorded outcomes.
func TestToolWindowObservation_CreateIsIdempotentAndPreservesProgress(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("codex", "s1", "t1", "call1")
	created, err := CreateToolWindowObservation(k, pendingRecord())
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v, want created", created, err)
	}
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationObserved}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// A second creator preserves the frozen set and receives created=false.
	clobber := pendingRecord()
	clobber.Candidates = []ObservedCandidate{{RepoPath: "/repos/other", RepositoryID: "x", Outcome: observationPending}}
	clobber.OmittedByCap = 0
	clobber.TotalActive = 1
	created, err = CreateToolWindowObservation(k, clobber)
	if err != nil || created {
		t.Fatalf("second create: created=%v err=%v, want not-created", created, err)
	}
	got, _ := LoadToolWindowObservation(k)
	if got == nil || len(got.Candidates) != 2 || got.Candidates[0].Outcome != observationObserved {
		t.Fatalf("frozen set/progress not preserved: %+v", got)
	}
}

// Checkpoints reject unknown candidates and outcomes.
func TestToolWindowObservation_CheckpointRejectsUnknownCandidate(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("codex", "s1", "t1", "call1")
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/ghost": observationObserved}); err == nil {
		t.Fatal("expected rejection of unknown candidate")
	}
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": "bogus"}); err == nil {
		t.Fatal("expected rejection of unknown outcome")
	}
}

func TestToolWindowObservation_ContentValidation(t *testing.T) {
	valid := pendingRecord()
	cases := map[string]func(*ToolWindowObservationRecord){
		"empty candidate identity": func(r *ToolWindowObservationRecord) { r.Candidates[0].RepositoryID = "" },
		"duplicate candidate": func(r *ToolWindowObservationRecord) {
			r.Candidates[1] = r.Candidates[0]
		},
		"partial primary": func(r *ToolWindowObservationRecord) { r.PrimaryRepositoryID = "" },
		"inconsistent counts": func(r *ToolWindowObservationRecord) { r.TotalActive = 99 },
		"unknown outcome": func(r *ToolWindowObservationRecord) { r.Candidates[0].Outcome = "weird" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := pendingRecord()
			_ = valid
			mutate(rec)
			if err := validateObservationRecord(rec); err == nil {
				t.Fatalf("%s: expected validation error", name)
			}
		})
	}
	// Commands outside a repository may have no primary target.
	outside := pendingRecord()
	outside.PrimaryRepoPath, outside.PrimaryRepositoryID = "", ""
	if err := validateObservationRecord(outside); err != nil {
		t.Fatalf("absent primary must be allowed: %v", err)
	}
}

// The stored identity must match the lookup key.
func TestToolWindowObservation_LoadRejectsInternalIdentityMismatch(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	t.Cleanup(func() { toolWindowNow = orig })
	toolWindowNow = func() int64 { return 10_000_000 }

	k := key("codex", "s1", "t1", "call1")
	path, err := toolWindowObservationPath(k)
	if err != nil {
		t.Fatal(err)
	}
	// Bypass creation to store a mismatched session ID at the expected path.
	rec := pendingRecord()
	rec.Version = toolWindowObservationVersion
	rec.CreatedAt = toolWindowNow()
	rec.Provider, rec.SessionID, rec.TurnID, rec.ToolUseID = "codex", "DIFFERENT", "t1", "call1"
	data, _ := json.Marshal(rec)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToolWindowObservation(k); err == nil {
		t.Fatal("expected internal identity-mismatch rejection")
	}
}

// Creation preserves invalid existing data and returns an error.
func TestToolWindowObservation_CreateRefusesInvalidExisting(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("codex", "s1", "t1", "call1")
	path, err := toolWindowObservationPath(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{not valid json`)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err == nil {
		t.Fatal("expected create to refuse a present-but-invalid manifest")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(corrupt) {
		t.Fatalf("invalid manifest was overwritten: %q", got)
	}
}

// Terminal outcomes cannot be reversed; identical retries are allowed.
func TestToolWindowObservation_CheckpointCannotReverseTerminal(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("codex", "s1", "t1", "call1")
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationObserved}); err != nil {
		t.Fatalf("pending->observed: %v", err)
	}
	// Identical retry is allowed.
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationObserved}); err != nil {
		t.Fatalf("identical retry rejected: %v", err)
	}
	// Reversal to pending is rejected.
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationPending}); err == nil {
		t.Fatal("expected rejection of observed->pending")
	}
	// Change to a different terminal is rejected.
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationSnapshotFailed}); err == nil {
		t.Fatal("expected rejection of observed->snapshot_failed")
	}
	got, _ := LoadToolWindowObservation(k)
	if got == nil || got.Candidates[0].Outcome != observationObserved {
		t.Fatalf("terminal outcome not preserved: %+v", got)
	}
}

// Deletion respects the lock and preserves its file identity.
func TestToolWindowObservation_DeleteRetainsLockAndSerializes(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("codex", "s1", "t1", "call1")
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatalf("create: %v", err)
	}
	path, err := toolWindowObservationPath(k)
	if err != nil {
		t.Fatal(err)
	}

	// Deletion fails while another caller holds the lock.
	orig := observationLockAttempts
	observationLockAttempts = 2
	t.Cleanup(func() { observationLockAttempts = orig })
	held, ok := lockObservation(path)
	if !ok {
		t.Fatal("could not acquire lock for test")
	}
	if err := DeleteToolWindowObservation(k); err == nil {
		_ = platform.UnlockFile(held)
		_ = held.Close()
		t.Fatal("expected delete to fail while lock is held")
	}
	_ = platform.UnlockFile(held)
	_ = held.Close()

	// Deletion removes only the manifest after the lock is released.
	if err := DeleteToolWindowObservation(k); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("manifest not removed: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock sidecar must be retained: %v", err)
	}
}

// stampIdentity sets manifest identity fields for direct persistence tests.
func stampIdentity(rec *ToolWindowObservationRecord, k toolWindowReceiptKey) *ToolWindowObservationRecord {
	rec.Version = toolWindowObservationVersion
	rec.Provider, rec.SessionID, rec.TurnID, rec.ToolUseID = k.Provider, k.SessionID, k.TurnID, k.ToolUseID
	return rec
}

// Cleanup removes stale manifests while retaining fresh manifests and lock files.
func TestSweepToolWindowObservations_RemovesStaleManifestRetainsLock(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	t.Cleanup(func() { toolWindowNow = orig })
	base := int64(1_000_000_000_000)
	toolWindowNow = func() int64 { return base }

	fresh := key("codex", "s-fresh", "t", "call")
	stale := key("codex", "s-stale", "t", "call")
	if _, err := CreateToolWindowObservation(fresh, pendingRecord()); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateToolWindowObservation(stale, pendingRecord()); err != nil {
		t.Fatal(err)
	}

	// Expire both manifests, then refresh one.
	toolWindowNow = func() int64 { return base + toolWindowTargetTTL.Milliseconds() + 1000 }
	freshPath, _ := toolWindowObservationPath(fresh)
	rec := pendingRecord()
	rec.CreatedAt = toolWindowNow()
	if err := writeObservation(freshPath, stampIdentity(rec, fresh)); err != nil {
		t.Fatal(err)
	}

	n, err := SweepToolWindowObservations()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1 (stale manifest only)", n)
	}
	stalePath, _ := toolWindowObservationPath(stale)
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale manifest not removed: %v", err)
	}
	// Manifest deletion preserves the lock file.
	if _, err := os.Stat(stalePath + ".lock"); err != nil {
		t.Fatalf("stale manifest's lock must be retained: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh manifest removed: %v", err)
	}
}

// A held lock prevents removal of its stale manifest.
func TestSweepToolWindowObservations_SkipsStaleManifestWhileLockHeld(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	origAttempts := observationLockAttempts
	t.Cleanup(func() {
		toolWindowNow = orig
		observationLockAttempts = origAttempts
	})
	base := int64(1_000_000_000_000)
	toolWindowNow = func() int64 { return base }

	k := key("codex", "s-stale", "t", "call")
	if _, err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatal(err)
	}
	path, _ := toolWindowObservationPath(k)

	// Hold the expired manifest's lock during cleanup.
	toolWindowNow = func() int64 { return base + toolWindowTargetTTL.Milliseconds() + 1000 }
	observationLockAttempts = 2
	held, ok := lockObservation(path)
	if !ok {
		t.Fatal("could not acquire lock for test")
	}
	defer func() { _ = platform.UnlockFile(held); _ = held.Close() }()

	n, err := SweepToolWindowObservations()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (locked manifest must be skipped)", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("locked manifest must be retained: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("held lock must be retained: %v", err)
	}
}

// Orphaned lock files are retained regardless of age.
func TestSweepToolWindowObservations_NeverReclaimsOrphanedLocks(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	dir, err := captureDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "obs-toolwindow-orphan.json.lock")
	if err := os.WriteFile(orphan, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Lock age must not trigger deletion.
	past := time.Now().Add(-2 * toolWindowTargetTTL)
	if err := os.Chtimes(orphan, past, past); err != nil {
		t.Fatal(err)
	}

	n, err := SweepToolWindowObservations()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (locks are never reclaimed)", n)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphaned lock must be retained: %v", err)
	}
}
