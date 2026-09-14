package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerStandaloneCaptureWaitReleasesLockAndCancels(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "cp", "commit", 100)
	started := make(chan struct{})
	original := workerProcess
	workerProcess = func(_ *WorkerService, ctx context.Context, _ WorkerInput) error {
		at := time.Now().Add(2 * time.Second)
		err := saveCheckpointCapture(ctx, h, &checkpointCapture{
			Version: 1, CheckpointID: "cp", RepositoryID: repoID, Through: 100,
			Deadline: at.UnixMilli(), Result: CaptureReadiness{Status: "pending"},
		})
		close(started)
		if err != nil {
			return err
		}
		return &captureNotSettled{At: at}
	}
	t.Cleanup(func() { workerProcess = original })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewWorkerService(nil).RunStandalone(ctx, WorkerInput{CheckpointID: "cp", RepoRoot: dir})
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("standalone worker did not stop")
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker did not reach capture check")
	}
	lock, err := acquireRepoLock(ctx, filepath.Join(dir, ".semantica"), "probe", time.Second)
	if err != nil {
		t.Fatalf("repository remained locked during capture wait: %v", err)
	}
	lock.release()
	cp, err := h.Queries.GetCheckpointByID(ctx, "cp")
	if err != nil || cp.LeaseOwner.Valid || cp.AttemptCount != 0 || cp.LastError.String != "capture_not_settled" {
		t.Fatalf("capture wait retained a lease or spent the failure budget: %+v, %v", cp, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("capture wait returned %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture wait ignored cancellation")
	}
}

func TestWorkerStandaloneLeavesOrdinaryRetryScheduled(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "cp", "commit", 100)
	ctx := context.Background()
	at := time.Now().Add(2 * time.Second).UnixMilli()
	if _, err := h.DB.ExecContext(ctx, "update checkpoints set next_attempt_at = ?, last_error = ? where checkpoint_id = ?", at, "upload failed", "cp"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := NewWorkerService(nil).RunStandalone(ctx, WorkerInput{CheckpointID: "cp", RepoRoot: dir})
	var scheduled *ErrRetryScheduled
	if !errors.As(err, &scheduled) || scheduled.CheckpointID != "cp" {
		t.Fatalf("ordinary retry behavior changed: %v", err)
	}
}
