package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func setupLockRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-lock", RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func findCheck(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q missing in %+v", id, checks)
	return Check{}
}

func TestCheckWorkerLock_NoLockFile(t *testing.T) {
	dir := setupLockRepo(t)
	checks := checkWorkerLock(context.Background(), Options{RepoPath: dir})
	lock := findCheck(t, checks, "lock")
	if lock.Status != StatusOK || !strings.Contains(lock.Message, "not held") {
		t.Errorf("no lock file: %+v", lock)
	}
}

func TestCheckWorkerLock_FreeLockFilePersists(t *testing.T) {
	dir := setupLockRepo(t)
	lockPath := service.WorkerLockPath(filepath.Join(dir, ".semantica"))
	if err := os.WriteFile(lockPath, []byte(`{"pid":1,"acquired_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checkWorkerLock(context.Background(), Options{RepoPath: dir})
	lock := findCheck(t, checks, "lock")
	if lock.Status != StatusOK || !strings.Contains(lock.Message, "free") {
		t.Errorf("released lock must probe as free, not stale: %+v", lock)
	}
	queue := findCheck(t, checks, "queue")
	if queue.Status != StatusOK || !strings.Contains(queue.Message, "no checkpoints waiting") {
		t.Errorf("empty queue: %+v", queue)
	}
}

func TestCheckWorkerLock_HeldByLiveProcess(t *testing.T) {
	dir := setupLockRepo(t)
	lockPath := service.WorkerLockPath(filepath.Join(dir, ".semantica"))
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := platform.LockFile(f); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.UnlockFile(f) }()
	meta, _ := json.Marshal(service.RepoLockInfo{
		PID: os.Getpid(), AcquiredAt: time.Now().UnixMilli(), CheckpointID: "ck-1",
	})
	if _, err := f.WriteAt(meta, 0); err != nil {
		t.Fatal(err)
	}

	checks := checkWorkerLock(context.Background(), Options{RepoPath: dir})
	lock := findCheck(t, checks, "lock")
	if lock.Status != StatusOK {
		t.Errorf("live short hold should be ok: %+v", lock)
	}
	if !strings.Contains(lock.Message, "held for") || !strings.Contains(lock.Message, "ck-1") {
		t.Errorf("holder diagnostics missing: %+v", lock)
	}
}

func TestCheckWorkerLock_DeadRecordedHolderWarns(t *testing.T) {
	dir := setupLockRepo(t)
	lockPath := service.WorkerLockPath(filepath.Join(dir, ".semantica"))
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := platform.LockFile(f); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.UnlockFile(f) }()
	meta, _ := json.Marshal(service.RepoLockInfo{PID: 1 << 30, AcquiredAt: time.Now().UnixMilli()})
	if _, err := f.WriteAt(meta, 0); err != nil {
		t.Fatal(err)
	}

	checks := checkWorkerLock(context.Background(), Options{RepoPath: dir})
	lock := findCheck(t, checks, "lock")
	if lock.Status != StatusWarn || !strings.Contains(lock.Message, "not alive") {
		t.Errorf("dead recorded holder should warn with evidence: %+v", lock)
	}
}

func TestCheckWorkerLock_BlockedQueueReportsFailedCheckpoint(t *testing.T) {
	dir := setupLockRepo(t)
	ctx := context.Background()
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	for i, cp := range []struct{ id, status string }{
		{"ck-failed", "failed"},
		{"ck-waiting", "pending"},
	} {
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: cp.id, RepositoryID: "repo-lock", CreatedAt: int64(i + 1),
			Kind: "auto", Status: cp.status,
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
			CommitHash: cp.id + "-c", RepositoryID: "repo-lock", CheckpointID: cp.id, LinkedAt: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	checks := checkWorkerLock(ctx, Options{RepoPath: dir})
	queue := findCheck(t, checks, "queue")
	if queue.Status != StatusWarn || !strings.Contains(queue.Message, "blocked by failed checkpoint ck-failed") {
		t.Errorf("blocked queue should name the blocker: %+v", queue)
	}
}

func TestCheckWorkerLock_QueueWaitingWithFreeLockWarns(t *testing.T) {
	dir := setupLockRepo(t)
	ctx := context.Background()
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-q", RepositoryID: "repo-lock", CreatedAt: 1,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "c1", RepositoryID: "repo-lock", CheckpointID: "ck-q", LinkedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	checks := checkWorkerLock(ctx, Options{RepoPath: dir})
	queue := findCheck(t, checks, "queue")
	if queue.Status != StatusWarn || !strings.Contains(queue.Message, "1 checkpoint(s) waiting") {
		t.Errorf("waiting queue with free lock should warn: %+v", queue)
	}
}

func TestCheckUnownedCaptureStates_WarnsForCwdlessState(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: "s-lost", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	checks := checkUnownedCaptureStates(context.Background())
	if len(checks) != 1 || checks[0].Status != StatusWarn {
		t.Fatalf("unowned state should warn: %+v", checks)
	}
	if !strings.Contains(checks[0].Message, "s-lost") {
		t.Errorf("warning should name the session: %s", checks[0].Message)
	}
}

func TestCheckUnownedCaptureStates_ReportsScopedDeferrals(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: "s-span", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1,
		CWD: "/some/repo", ScopedDeferrals: 3, LastDeferredAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	checks := checkUnownedCaptureStates(context.Background())
	deferred := findCheck(t, checks, "cross_repo_capture_states")
	if deferred.Status != StatusWarn || !strings.Contains(deferred.Message, "s-span") ||
		!strings.Contains(deferred.Message, "3 deferral(s)") {
		t.Errorf("scoped deferrals should be reported: %+v", deferred)
	}
}

func TestCheckUnownedCaptureStates_ReportsOrphanedSegments(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: "s-gone", StateKey: "s-gone.orphan.1", Provider: "claude-code",
		TranscriptRef: "old-t", TranscriptOffset: 7, Timestamp: 1,
		ScopedDeferrals: 2, OrphanedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}

	checks := checkUnownedCaptureStates(context.Background())
	orphan := findCheck(t, checks, "orphaned_capture_segments")
	if orphan.Status != StatusWarn || !strings.Contains(orphan.Message, "s-gone") {
		t.Errorf("orphaned segment should warn: %+v", orphan)
	}
	for _, c := range checks {
		if c.ID == "cross_repo_capture_states" {
			t.Errorf("orphan must not double-report as an active deferral: %+v", c)
		}
	}
}
