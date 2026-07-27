package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func setupQueueRepo(t *testing.T) (string, *sqlstore.Handle, string) {
	t.Helper()
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })
	repoID := "repo-serial"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return dir, h, repoID
}

func insertPendingLinked(t *testing.T, h *sqlstore.Handle, repoID, cpID, commit string, createdAt int64) {
	t.Helper()
	ctx := context.Background()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID, CreatedAt: createdAt,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commit, RepositoryID: repoID, CheckpointID: cpID, LinkedAt: createdAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func stubWorkerProcess(t *testing.T, h *sqlstore.Handle, got *[]string) {
	t.Helper()
	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		*got = append(*got, in.CheckpointID)
		return h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
			ManifestHash: sql.NullString{String: "h", Valid: true},
			SizeBytes:    sql.NullInt64{Int64: 0, Valid: true},
			CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
			CheckpointID: in.CheckpointID,
		})
	}
	t.Cleanup(func() { workerProcess = orig })
}

func TestWorkerRun_DrainsInRepositorySequenceOrder(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)

	insertPendingLinked(t, h, repoID, "ck-c", "c1", 100)
	insertPendingLinked(t, h, repoID, "ck-a", "c2", 100)
	insertPendingLinked(t, h, repoID, "ck-b", "c3", 100)

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	if err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-a", RepoRoot: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"ck-c", "ck-a", "ck-b"}
	if len(got) != len(want) {
		t.Fatalf("processed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("processing order %v, want %v (repository_sequence)", got, want)
		}
	}
}

func TestWorkerRun_DrainObservesWorkInsertedMidDrain(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-first", "c1", 100)

	var got []string
	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		got = append(got, in.CheckpointID)
		if in.CheckpointID == "ck-first" {
			insertPendingLinked(t, h, repoID, "ck-late", "c2", 200)
		}
		return h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
			ManifestHash: sql.NullString{String: "h", Valid: true},
			SizeBytes:    sql.NullInt64{Int64: 0, Valid: true},
			CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
			CheckpointID: in.CheckpointID,
		})
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	if err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-first", RepoRoot: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 2 || got[0] != "ck-first" || got[1] != "ck-late" {
		t.Fatalf("processed %v, want [ck-first ck-late]", got)
	}
}

func TestWorkerRun_StopsAfterFailedCheckpoint(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-1", "c1", 100)
	insertPendingLinked(t, h, repoID, "ck-2", "c2", 100)

	var got []string
	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		got = append(got, in.CheckpointID)
		if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
			CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
			CheckpointID: in.CheckpointID,
		}); err != nil {
			t.Errorf("mark failed: %v", err)
		}
		return errors.New("injected enrichment failure")
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	if err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-1", RepoRoot: dir}); err == nil {
		t.Fatal("Run should propagate the processing failure")
	}
	if len(got) != 1 || got[0] != "ck-1" {
		t.Fatalf("processed %v, want only ck-1", got)
	}
}

func TestWorkerRun_FailedHeadBlocksNextInvocation(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-1", "c1", 100)
	insertPendingLinked(t, h, repoID, "ck-2", "c2", 100)
	if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: "ck-1",
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	if err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-2", CommitHash: "c2", RepoRoot: dir}); err != nil {
		t.Fatalf("blocked queue must exit successfully (marker removable): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("blocked queue processed %v, want nothing", got)
	}
	cp, err := h.Queries.GetCheckpointByID(ctx, "ck-2")
	if err != nil {
		t.Fatal(err)
	}
	if cp.Status != "pending" {
		t.Fatalf("ck-2 status = %q, want pending (blocked behind failed ck-1)", cp.Status)
	}
}

func TestWorkerRun_PassedFailedCheckpointDoesNotBlock(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-old", "c1", 100)
	if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: "ck-old",
	}); err != nil {
		t.Fatal(err)
	}
	insertPendingLinked(t, h, repoID, "ck-mid", "c2", 200)
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: "h", Valid: true},
		SizeBytes:    sql.NullInt64{Int64: 0, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: "ck-mid",
	}); err != nil {
		t.Fatal(err)
	}
	insertPendingLinked(t, h, repoID, "ck-new", "c3", 300)

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	if err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-new", CommitHash: "c3", RepoRoot: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0] != "ck-new" {
		t.Fatalf("processed %v, want [ck-new] (passed failure must not block)", got)
	}
}

func TestWorkerRun_LockTimeoutIsRetryableWhilePending(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-wait", "c1", 100)
	semDir := filepath.Join(dir, ".semantica")

	holder, err := acquireRepoLock(context.Background(), semDir, "other", time.Second)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	defer holder.release()

	origWait := repoLockWait
	repoLockWait = 300 * time.Millisecond
	t.Cleanup(func() { repoLockWait = origWait })

	svc := NewWorkerService(nil)
	err = svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-wait", RepoRoot: dir})
	if err == nil {
		t.Fatal("Run must not report success on lock timeout with pending checkpoint")
	}
	if !errors.Is(err, errRepoLockTimeout) {
		t.Fatalf("error should wrap the lock timeout: %v", err)
	}
}

func TestWorkerRun_LockTimeoutSettledCheckpointSucceeds(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-done", "c1", 100)
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: "h", Valid: true},
		SizeBytes:    sql.NullInt64{Int64: 0, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: "ck-done",
	}); err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(dir, ".semantica")

	holder, err := acquireRepoLock(ctx, semDir, "other", time.Second)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	defer holder.release()

	origWait := repoLockWait
	repoLockWait = 300 * time.Millisecond
	t.Cleanup(func() { repoLockWait = origWait })

	svc := NewWorkerService(nil)
	if err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-done", RepoRoot: dir}); err != nil {
		t.Fatalf("settled checkpoint should exit successfully: %v", err)
	}
}
