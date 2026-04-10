package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TestTidy_DryRunDoesNotMutate verifies that dry-run reports stale checkpoints
// without changing their status.
func TestTidy_DryRunDoesNotMutate(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)
	insertStaleCheckpoint(t, ctx, dir, "stale-cp-1", time.Now().Add(-2*time.Hour).UnixMilli())

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{RepoPath: dir, Apply: false})
	if err != nil {
		t.Fatalf("Tidy dry-run: %v", err)
	}

	if !res.DryRun {
		t.Error("expected DryRun=true")
	}
	if res.CheckpointsMarked != 1 {
		t.Errorf("CheckpointsMarked = %d, want 1", res.CheckpointsMarked)
	}

	cp := getCheckpointStatus(t, ctx, dir, "stale-cp-1")
	if cp != "pending" {
		t.Errorf("checkpoint status = %q after dry-run, want pending", cp)
	}
}

// TestTidy_ApplyMarksStaleCheckpointsFailed verifies that --apply marks
// stale pending checkpoints as failed.
func TestTidy_ApplyMarksStaleCheckpointsFailed(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)
	insertStaleCheckpoint(t, ctx, dir, "stale-cp-2", time.Now().Add(-2*time.Hour).UnixMilli())

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{RepoPath: dir, Apply: true})
	if err != nil {
		t.Fatalf("Tidy apply: %v", err)
	}

	if res.DryRun {
		t.Error("expected DryRun=false")
	}
	if res.CheckpointsMarked != 1 {
		t.Errorf("CheckpointsMarked = %d, want 1", res.CheckpointsMarked)
	}

	cp := getCheckpointStatus(t, ctx, dir, "stale-cp-2")
	if cp != "failed" {
		t.Errorf("checkpoint status = %q after apply, want failed", cp)
	}
}

// TestTidy_CompleteCheckpointsNotTouched verifies that complete checkpoints
// are never affected by tidy.
func TestTidy_CompleteCheckpointsNotTouched(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)

	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "complete-cp",
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    time.Now().Add(-3 * time.Hour).UnixMilli(),
		Kind:         "auto",
		Status:       "complete",
		ManifestHash: sql.NullString{String: "abc123", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{RepoPath: dir, Apply: true})
	if err != nil {
		t.Fatalf("Tidy: %v", err)
	}
	if res.CheckpointsMarked != 0 {
		t.Errorf("CheckpointsMarked = %d, want 0 (complete checkpoint should not be touched)", res.CheckpointsMarked)
	}

	cp := getCheckpointStatus(t, ctx, dir, "complete-cp")
	if cp != "complete" {
		t.Errorf("checkpoint status = %q, want complete", cp)
	}
}

// TestTidy_CommitLinkedPendingNotTouched verifies that pending checkpoints
// with a commit link are not marked failed.
func TestTidy_CommitLinkedPendingNotTouched(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)

	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "linked-pending-cp",
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    time.Now().Add(-3 * time.Hour).UnixMilli(),
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "deadbeef",
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: "linked-pending-cp",
		LinkedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{RepoPath: dir, Apply: true})
	if err != nil {
		t.Fatalf("Tidy: %v", err)
	}
	if res.CheckpointsMarked != 0 {
		t.Errorf("CheckpointsMarked = %d, want 0 (commit-linked should not be touched)", res.CheckpointsMarked)
	}
}

// TestTidy_DryRunDoesNotMutateBroker verifies that dry-run does not
// prune stale broker registry entries.
func TestTidy_DryRunDoesNotMutateBroker(t *testing.T) {
	ctx := context.Background()

	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	staleRepo := filepath.Join(t.TempDir(), "gone-repo")
	reg := struct {
		Repos []broker.RegisteredRepo `json:"repos"`
	}{
		Repos: []broker.RegisteredRepo{
			{
				RepoID:        "fake-id",
				Path:          staleRepo,
				CanonicalPath: staleRepo,
				EnabledAt:     time.Now().Add(-24 * time.Hour).UnixMilli(),
				Active:        true,
			},
		},
	}
	regData, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(home, "repos.json"), regData, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{Apply: false})
	if err != nil {
		t.Fatalf("Tidy dry-run: %v", err)
	}

	if res.BrokerEntriesPruned != 1 {
		t.Errorf("BrokerEntriesPruned = %d, want 1 (should report stale entry)", res.BrokerEntriesPruned)
	}

	data, err := os.ReadFile(filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after struct {
		Repos []broker.RegisteredRepo `json:"repos"`
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Repos) != 1 {
		t.Errorf("registry has %d entries after dry-run, want 1 (should not be pruned)", len(after.Repos))
	}
}

// TestTidy_EmptyTranscriptRefCaptureStatePreserved verifies that capture
// states with an empty TranscriptRef are preserved even when old.
func TestTidy_EmptyTranscriptRefCaptureStatePreserved(t *testing.T) {
	ctx := context.Background()

	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	// Create a capture state with empty TranscriptRef, older than the threshold.
	state := &hooks.CaptureState{
		SessionID:     "old-session-no-ref",
		Provider:      "claude_code",
		TranscriptRef: "",
		Timestamp:     time.Now().Add(-48 * time.Hour).UnixMilli(),
	}
	if err := hooks.SaveCaptureState(state); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	svc := NewTidyService()
	res, err := svc.Tidy(ctx, TidyInput{Apply: true})
	if err != nil {
		t.Fatalf("Tidy: %v", err)
	}

	if res.CaptureStatesRemoved != 0 {
		t.Errorf("CaptureStatesRemoved = %d, want 0 (empty TranscriptRef should be preserved)", res.CaptureStatesRemoved)
	}

	// Verify the state file still exists.
	loaded, err := hooks.LoadCaptureState("old-session-no-ref")
	if err != nil {
		t.Errorf("capture state was deleted: %v", err)
	}
	if loaded == nil {
		t.Error("capture state should still exist")
	}
}

// helpers

func insertStaleCheckpoint(t *testing.T, ctx context.Context, dir, cpID string, createdAt int64) {
	t.Helper()
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    createdAt,
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatal(err)
	}
}

func getCheckpointStatus(t *testing.T, ctx context.Context, dir, cpID string) string {
	t.Helper()
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	cp, err := h.Queries.GetCheckpointByID(ctx, cpID)
	if err != nil {
		t.Fatalf("get checkpoint %s: %v", cpID, err)
	}
	return cp.Status
}
