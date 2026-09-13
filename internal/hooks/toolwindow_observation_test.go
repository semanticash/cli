package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	if err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
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
	if err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := CheckpointToolWindowObservation(k, map[string]string{"/repos/cli": observationObserved}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// A different candidate set cannot replace the existing manifest.
	clobber := pendingRecord()
	clobber.Candidates = []ObservedCandidate{{RepoPath: "/repos/other", RepositoryID: "x", Outcome: observationPending}}
	clobber.OmittedByCap = 0
	clobber.TotalActive = 1
	if err := CreateToolWindowObservation(k, clobber); err != nil {
		t.Fatalf("second create: %v", err)
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
	if err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
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
	if err := CreateToolWindowObservation(k, pendingRecord()); err == nil {
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
	if err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
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
	if err := CreateToolWindowObservation(k, pendingRecord()); err != nil {
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
