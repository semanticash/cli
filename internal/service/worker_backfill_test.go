package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// TestWorkerRun_SkipsDrainAfterLivePushRetry runs the worker with a commit
// that triggers a live pushAttribution. Since there's no real server, the
// push fails with PushRetry. The test verifies that the pending backfill
// state is NOT drained in the same checkpoint cycle (cursor stays at zero),
// proving the livePushRetried skip path works.
func TestWorkerRun_SkipsDrainAfterLivePushRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Isolate from real global state.
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	// Point endpoint at a port that refuses connections so the push fails
	// fast with a retryable network error, not a slow timeout.
	t.Setenv("SEMANTICA_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("SEMANTICA_API_KEY", "test-key")

	// Set up a git repo with one committed file.
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")
	commitHash := gitRun(t, dir, "rev-parse", "HEAD")

	// Enable semantica (creates .semantica dir, DB, baseline checkpoint).
	enableSemantica(t, ctx, dir)

	semDir := filepath.Join(dir, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	// Write settings with connected=true.
	connectedRepoID := "hosted-" + uuid.NewString()
	s, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	s.Connected = true
	s.ConnectedRepoID = connectedRepoID
	if err := util.WriteSettings(semDir, s); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	// Open the DB to create fixtures.
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Get the local repo row (created by enable).
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}

	// Create a pending checkpoint linked to the real commit.
	now := time.Now().UnixMilli()
	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    now,
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	// Insert a commit link for the real commit (as post-commit would).
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: cpID,
		LinkedAt:     now,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// Create an older historical commit link to serve as backfill candidate.
	olderCpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: olderCpID,
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    now - 100_000,
		Kind:         "auto",
		Status:       "complete",
	}); err != nil {
		t.Fatalf("insert older checkpoint: %v", err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "older-historical-commit",
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: olderCpID,
		LinkedAt:     now - 100_000,
	}); err != nil {
		t.Fatalf("insert older commit link: %v", err)
	}

	// Seed pending backfill state with the older commit as the cutoff.
	// The cursor starts at zero, so "older-historical-commit" is a candidate.
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID:  connectedRepoID,
		RepositoryID:     repoRow.RepositoryID,
		CutoffLinkedAt:   now - 100_000,
		CutoffCommitHash: "older-historical-commit",
		UpdatedAt:        now,
	}); err != nil {
		t.Fatalf("upsert backfill: %v", err)
	}

	_ = sqlstore.Close(h)

	// Run the worker. The live push for commitHash will fail (no server)
	// with PushRetry, which should prevent the backfill drain from running.
	ws := NewWorkerService()
	_ = ws.Run(ctx, WorkerInput{
		CheckpointID: cpID,
		CommitHash:   commitHash,
		RepoRoot:     dir,
	})

	// Re-open DB and verify backfill state.
	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	bf, err := h.Queries.GetAttributionBackfill(ctx, connectedRepoID)
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}

	// The cursor should NOT have advanced - drain was skipped because
	// the live push failed with a retryable error in the same cycle.
	if bf.CursorLinkedAt != 0 || bf.CursorCommitHash != "" {
		t.Errorf("backfill cursor should not have advanced (drain should be skipped after live retry), got (%d, %q)",
			bf.CursorLinkedAt, bf.CursorCommitHash)
	}

	// The cutoff SHOULD have been extended to include the failed live commit.
	if bf.CutoffLinkedAt < now {
		t.Errorf("backfill cutoff should have been extended to include the failed live commit, got cutoff_linked_at=%d (now=%d)",
			bf.CutoffLinkedAt, now)
	}

	if bf.Status != "pending" {
		t.Errorf("backfill should still be pending, got %s", bf.Status)
	}
}

// TestWorkerRun_DrainsBackfillWhenNoPushRetry verifies that the backfill IS
// drained when there's no live push failure (commit hash is empty but the
// repo is connected with pending backfill). This is the positive counterpart
// to the skip test above.
func TestWorkerRun_DrainsBackfillWhenNoPushRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Isolate from real global state.
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	// Point to a refused port - drain will try to push historical commits
	// and fail, but the important thing is that drain WAS attempted (the
	// failure gets recorded in backfill state).
	t.Setenv("SEMANTICA_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("SEMANTICA_API_KEY", "test-key")

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")

	enableSemantica(t, ctx, dir)

	semDir := filepath.Join(dir, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	connectedRepoID := "hosted-" + uuid.NewString()
	s, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	s.Connected = true
	s.ConnectedRepoID = connectedRepoID
	if err := util.WriteSettings(semDir, s); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}

	now := time.Now().UnixMilli()
	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    now,
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	// Historical commit link as a backfill candidate.
	olderCpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: olderCpID,
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    now - 100_000,
		Kind:         "auto",
		Status:       "complete",
	}); err != nil {
		t.Fatalf("insert older checkpoint: %v", err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "older-historical-commit",
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: olderCpID,
		LinkedAt:     now - 100_000,
	}); err != nil {
		t.Fatalf("insert older commit link: %v", err)
	}

	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID:  connectedRepoID,
		RepositoryID:     repoRow.RepositoryID,
		CutoffLinkedAt:   now - 100_000,
		CutoffCommitHash: "older-historical-commit",
		UpdatedAt:        now,
	}); err != nil {
		t.Fatalf("upsert backfill: %v", err)
	}

	_ = sqlstore.Close(h)

	// Run the worker WITHOUT a commit hash - no live push, so the
	// livePushRetried flag stays false, and drain SHOULD run.
	ws := NewWorkerService()
	if err := ws.Run(ctx, WorkerInput{
		CheckpointID: cpID,
		CommitHash:   "", // no commit -> no live push -> drain not skipped
		RepoRoot:     dir,
	}); err != nil {
		t.Fatalf("run worker: %v", err)
	}

	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	bf, err := h.Queries.GetAttributionBackfill(ctx, connectedRepoID)
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}

	// Drain should have attempted to push "older-historical-commit".
	// Since there's no server, it will fail - but the attempt itself proves
	// the drain ran. Check for either: cursor advanced (push succeeded -
	// won't happen here) or failure state recorded (push failed).
	drainRan := bf.CursorLinkedAt != 0 || (bf.FailedCommitHash.Valid && bf.FailedCommitHash.String == "older-historical-commit")
	if !drainRan {
		t.Errorf("backfill drain should have run (no live push retry), but no evidence of drain: cursor=(%d,%q) failed=%v",
			bf.CursorLinkedAt, bf.CursorCommitHash, bf.FailedCommitHash)
	}
}
