package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/launcher"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// readCheckpoint re-reads a checkpoint row.
func readCheckpoint(t *testing.T, h *sqlstore.Handle, id string) sqldb.Checkpoint {
	t.Helper()
	cp, err := h.Queries.GetCheckpointByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// Transient failures release the lease and schedule a retry.
func TestWorkerRun_TransientFailureSchedulesRetry(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-t", "c1", 100)

	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		return errors.New("injected transient failure")
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	if err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-t", RepoRoot: dir}); err == nil {
		t.Fatal("Run should surface the failure")
	}

	cp := readCheckpoint(t, h, "ck-t")
	if cp.Status != "pending" {
		t.Fatalf("status = %q, want pending (transient)", cp.Status)
	}
	if cp.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1", cp.AttemptCount)
	}
	if cp.LastError.String == "" || cp.NextAttemptAt <= time.Now().UnixMilli() {
		t.Fatalf("retry metadata missing: last_error=%q next_attempt_at=%d", cp.LastError.String, cp.NextAttemptAt)
	}
	if cp.LeaseOwner.Valid {
		t.Fatalf("lease not released: %+v", cp.LeaseOwner)
	}

	// A second run must stop at the scheduled retry, not report success.
	err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-t", RepoRoot: dir})
	var sched *ErrRetryScheduled
	if !errors.As(err, &sched) {
		t.Fatalf("second run = %v, want ErrRetryScheduled", err)
	}
	if readCheckpoint(t, h, "ck-t").AttemptCount != 1 {
		t.Fatal("waiting on a scheduled retry must not consume an attempt")
	}
}

// Exhausted retries preserve the final error and fail terminally.
func TestWorkerRun_AttemptsExhaustTerminally(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-x", "c1", 100)

	origDelay := retryInitialDelay
	retryInitialDelay = time.Millisecond
	t.Cleanup(func() { retryInitialDelay = origDelay })

	calls := 0
	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		calls++
		return fmt.Errorf("injected failure %d", calls)
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	for i := 0; i < retryMaxAttempts; i++ {
		_ = svc.Run(ctx, WorkerInput{CheckpointID: "ck-x", RepoRoot: dir})
		time.Sleep(5 * time.Millisecond) // let the tiny backoff elapse
	}

	cp := readCheckpoint(t, h, "ck-x")
	if cp.Status != "failed" {
		t.Fatalf("status = %q after %d attempts, want failed", cp.Status, calls)
	}
	if cp.AttemptCount != retryMaxAttempts || calls != retryMaxAttempts {
		t.Fatalf("attempts = %d (calls %d), want %d", cp.AttemptCount, calls, retryMaxAttempts)
	}
	if cp.LastError.String != fmt.Sprintf("injected failure %d", retryMaxAttempts) {
		t.Fatalf("last_error = %q, want the final attempt's error", cp.LastError.String)
	}
}

// Permanent errors skip the retry budget.
func TestWorkerRun_PermanentErrorFailsImmediately(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-p", "c1", 100)

	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		return markPermanent(errors.New("corrupt immutable checkpoint data"))
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	if err := svc.Run(context.Background(), WorkerInput{CheckpointID: "ck-p", RepoRoot: dir}); err == nil {
		t.Fatal("Run should surface the failure")
	}
	cp := readCheckpoint(t, h, "ck-p")
	if cp.Status != "failed" || cp.AttemptCount != 1 {
		t.Fatalf("permanent error should fail terminally on attempt 1: status=%q attempts=%d", cp.Status, cp.AttemptCount)
	}
}

// A live foreign lease stops the drain without consuming an attempt.
func TestWorkerRun_LiveLeaseStopsDrain(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-l", "c1", 100)
	if _, err := h.DB.ExecContext(ctx,
		"update checkpoints set lease_owner = 'other-host:1:dead', lease_until = ? where checkpoint_id = 'ck-l'",
		time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-l", RepoRoot: dir})
	var held *ErrLeaseHeld
	if !errors.As(err, &held) {
		t.Fatalf("run = %v, want ErrLeaseHeld", err)
	}
	if len(got) != 0 {
		t.Fatalf("processed %v under a live foreign lease", got)
	}
	if readCheckpoint(t, h, "ck-l").AttemptCount != 0 {
		t.Fatal("honoring a lease must not consume an attempt")
	}
}

// An expired lease is reclaimed with one attempt increment.
func TestWorkerRun_ExpiredLeaseReclaims(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-e", "c1", 100)
	// A crashed worker left attempt 1 claimed and its lease expired.
	if _, err := h.DB.ExecContext(ctx,
		"update checkpoints set lease_owner = 'crashed:1:x', lease_until = ?, attempt_count = 1 where checkpoint_id = 'ck-e'",
		time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	if err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-e", RepoRoot: dir}); err != nil {
		t.Fatalf("reclaim run: %v", err)
	}
	if len(got) != 1 || got[0] != "ck-e" {
		t.Fatalf("processed %v, want [ck-e]", got)
	}
	cp := readCheckpoint(t, h, "ck-e")
	if cp.Status != "complete" || cp.AttemptCount != 2 {
		t.Fatalf("reclaim outcome: status=%q attempts=%d, want complete/2", cp.Status, cp.AttemptCount)
	}
}

// Manual retry resets the attempt budget and drains the checkpoint.
func TestRetryCheckpoint_ManualResetsBudgetAndRuns(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-m", "c1", 100)
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "boom", Valid: true},
		CheckpointID: "ck-m",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.ExecContext(ctx,
		"update checkpoints set attempt_count = 5 where checkpoint_id = 'ck-m'"); err != nil {
		t.Fatal(err)
	}

	var got []string
	stubWorkerProcess(t, h, &got)

	svc := NewWorkerService(nil)
	id, err := svc.ResolveAndRetryCheckpoint(ctx, dir, "ck-m")
	if err != nil {
		t.Fatalf("manual retry: %v", err)
	}
	if id != "ck-m" {
		t.Fatalf("resolved %q", id)
	}
	cp := readCheckpoint(t, h, "ck-m")
	if cp.Status != "complete" || cp.AttemptCount != 1 {
		t.Fatalf("after manual retry: status=%q attempts=%d, want complete/1 (budget reset)", cp.Status, cp.AttemptCount)
	}
	if len(got) != 1 || got[0] != "ck-m" {
		t.Fatalf("processed %v, want [ck-m]", got)
	}
}

// Manual retry accepts only terminally failed checkpoints.
func TestRetryCheckpoint_RejectsNonFailed(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-ok", "c1", 100)

	svc := NewWorkerService(nil)
	if _, err := svc.ResolveAndRetryCheckpoint(context.Background(), dir, "ck-ok"); err == nil {
		t.Fatal("retrying a pending checkpoint should be rejected")
	}
}

// A checkpoint claim is atomic across competing writers.
func TestClaimCheckpoint_ConcurrentClaimsSingleWinner(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-race", "c1", 100)
	_ = h

	const n = 8
	wins := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			hi, err := sqlstore.Open(ctx, dir+"/.semantica/lineage.db", sqlstore.UserFacingOpenOptions())
			if err != nil {
				wins <- false
				return
			}
			defer func() { _ = sqlstore.Close(hi) }()
			_, err = hi.Queries.ClaimCheckpoint(ctx, sqldb.ClaimCheckpointParams{
				LeaseOwner:   sql.NullString{String: fmt.Sprintf("racer-%d", i), Valid: true},
				LeaseUntil:   sql.NullInt64{Int64: time.Now().Add(time.Minute).UnixMilli(), Valid: true},
				CheckpointID: "ck-race",
				Now:          time.Now().UnixMilli(),
			})
			wins <- err == nil
		}(i)
	}
	winners := 0
	for i := 0; i < n; i++ {
		if <-wins {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if readCheckpoint(t, h, "ck-race").AttemptCount != 1 {
		t.Fatal("attempt_count must increment exactly once")
	}
}

// A near transient retry completes within one drain invocation.
func TestDrainUntilStable_TransientFailureRetriesInProcess(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	setupDrainEnv(t, dir)
	insertPendingLinked(t, h, repoID, "ck-int", "c1", 100)
	markerPath := writeMarker(t, dir, "ck-int")

	origDelay := retryInitialDelay
	retryInitialDelay = 50 * time.Millisecond
	t.Cleanup(func() { retryInitialDelay = origDelay })

	calls := 0
	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		calls++
		if calls == 1 {
			return errors.New("injected transient failure")
		}
		return h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
			ManifestHash: sql.NullString{String: "h", Valid: true},
			SizeBytes:    sql.NullInt64{Int64: 0, Valid: true},
			CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
			CheckpointID: in.CheckpointID,
		})
	}
	t.Cleanup(func() { workerProcess = orig })

	if err := DrainUntilStable(context.Background(), 0, NewWorkerService(nil).Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2 (failure + in-process retry)", calls)
	}
	if cp := readCheckpoint(t, h, "ck-int"); cp.Status != "complete" {
		t.Fatalf("status = %q, want complete", cp.Status)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker should be deleted after the successful retry: %v", err)
	}
}

// A retried checkpoint keeps its marker when it fails transiently.
func TestRetryCheckpoint_TransientFailureLeavesDurableMarker(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	setupDrainEnv(t, dir)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-mm", "c1", 100)
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "boom", Valid: true},
		CheckpointID: "ck-mm",
	}); err != nil {
		t.Fatal(err)
	}

	orig := workerProcess
	workerProcess = func(s *WorkerService, ctx context.Context, in WorkerInput) error {
		return errors.New("still failing")
	}
	t.Cleanup(func() { workerProcess = orig })

	svc := NewWorkerService(nil)
	_, err := svc.ResolveAndRetryCheckpoint(ctx, dir, "ck-mm")
	var sched *ErrRetryScheduled
	if !errors.As(err, &sched) {
		t.Fatalf("retry outcome = %v, want ErrRetryScheduled", err)
	}

	markers, err := launcher.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 {
		t.Fatalf("markers = %v, want the restored wake-up marker", markers)
	}
	cp := readCheckpoint(t, h, "ck-mm")
	if cp.Status != "pending" || cp.NextAttemptAt == 0 {
		t.Fatalf("retried checkpoint: status=%q next_attempt_at=%d, want pending with schedule", cp.Status, cp.NextAttemptAt)
	}
}

// A marker-write failure leaves the checkpoint terminally failed.
func TestRetryCheckpoint_MarkerFailureKeepsFailedState(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-mf", "c1", 100)
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "boom", Valid: true},
		CheckpointID: "ck-mf",
	}); err != nil {
		t.Fatal(err)
	}
	// Marker writes go to <repo>/.semantica/pending; a file at that
	// path makes them fail.
	pendingDir := filepath.Join(dir, ".semantica", "pending")
	if err := os.WriteFile(pendingDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(pendingDir) })

	svc := NewWorkerService(nil)
	if _, err := svc.ResolveAndRetryCheckpoint(ctx, dir, "ck-mf"); err == nil {
		t.Fatal("retry should fail when the marker cannot be written")
	}
	cp := readCheckpoint(t, h, "ck-mf")
	if cp.Status != "failed" {
		t.Fatalf("status = %q, want failed (still retryable)", cp.Status)
	}

	// With the marker path restored, the same command succeeds.
	if err := os.Remove(pendingDir); err != nil {
		t.Fatal(err)
	}
	var got []string
	stubWorkerProcess(t, h, &got)
	if _, err := svc.ResolveAndRetryCheckpoint(ctx, dir, "ck-mf"); err != nil {
		t.Fatalf("retry after fixing marker path: %v", err)
	}
	if cp := readCheckpoint(t, h, "ck-mf"); cp.Status != "complete" {
		t.Fatalf("status = %q, want complete", cp.Status)
	}
}

// Rejected retries do not write markers.
func TestRetryCheckpoint_NonFailedWritesNoMarker(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "ck-nf", "c1", 100)

	svc := NewWorkerService(nil)
	if _, err := svc.ResolveAndRetryCheckpoint(context.Background(), dir, "ck-nf"); err == nil {
		t.Fatal("retrying a pending checkpoint should be rejected")
	}
	markers, err := launcher.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Fatalf("no marker should be written for a rejected retry: %v", markers)
	}
}
