package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type WorkerService struct {
	registry *hooks.Registry
}

// NewWorkerService constructs the worker with the given hook
// registry. The registry drives reconciliation of active capture
// sessions; production callers must pass
// providers.NewHookRegistry() (the worker cobra command does so).
// A nil registry makes reconciliation a no-op: every per-session
// provider lookup returns nil and is skipped. This is useful only
// for tests that intentionally exercise the non-reconcile paths.
func NewWorkerService(registry *hooks.Registry) *WorkerService {
	return &WorkerService{registry: registry}
}

type WorkerInput struct {
	CheckpointID string
	CommitHash   string // optional, for logging
	RepoRoot     string
}

// workerContext bundles the shared handles opened during checkpoint
// preparation.
type workerContext struct {
	h         *sqlstore.Handle
	blobStore *blobs.Store
	repo      *git.Repo
	cp        sqldb.Checkpoint
	semDir    string
}

func (wc *workerContext) close() { _ = sqlstore.Close(wc.h) }

// prepareResult is the result of prepareCheckpoint.
type prepareResult struct {
	wctx *workerContext
	skip bool // true when checkpoint is already complete/failed, not found, or semantica disabled
}

// wlogWriter is the destination for worker log lines. It defaults to
// os.Stderr, which the detached worker and launchd plist route to
// different files. The drain loop swaps it per job so job-level output
// lands in <repo>/.semantica/worker.log while launcher-level output
// stays on the launcher log. Callers must treat it as single-goroutine.
var wlogWriter io.Writer = os.Stderr

func wlog(format string, args ...any) {
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(wlogWriter, "%s  %s", ts, msg)
}

// RedirectWorkerLog opens path in append mode and routes worker logs
// there. Linux and Windows launchers use it; macOS launchd already
// redirects output at the OS level.
//
// The redirect updates wlogWriter, os.Stdout, os.Stderr, and the
// default slog logger so plain writes and structured logs land in the
// same file. It does not retarget loggers that captured os.Stderr at
// package init in other code, and it does not affect runtime panic
// output.
//
// Call this before per-job redirects in `worker drain`. The returned
// cleanup restores the previous logging state and closes the file. It
// is safe to call cleanup multiple times.
func RedirectWorkerLog(path string) (cleanup func() error, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("redirect worker log: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("redirect worker log: open %q: %w", path, err)
	}

	prevWlog := wlogWriter
	prevStdout := os.Stdout
	prevStderr := os.Stderr

	wlogWriter = f
	os.Stdout = f
	os.Stderr = f
	restoreSlog := setSlogDefaultTo(f)

	closed := false
	return func() error {
		restoreSlog()
		wlogWriter = prevWlog
		os.Stdout = prevStdout
		os.Stderr = prevStderr
		if closed {
			return nil
		}
		closed = true
		return f.Close()
	}, nil
}

// setSlogDefaultTo installs a default slog logger that writes to w and
// returns a restore function.
//
// Go's slog.SetDefault rewires the standard log package when the new
// handler is not the runtime default: it changes both log output and
// log flags. Restoring the previous slog logger does not undo those
// changes, so we snapshot and restore the standard logger state here
// as well.
func setSlogDefaultTo(w io.Writer) func() {
	prevSlog := slog.Default()
	prevLogWriter := stdlog.Writer()
	prevLogFlags := stdlog.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return func() {
		slog.SetDefault(prevSlog)
		stdlog.SetOutput(prevLogWriter)
		stdlog.SetFlags(prevLogFlags)
	}
}

// prepareCheckpoint opens the DB, validates the checkpoint is pending,
// initializes the blob store, and opens the git repo. Returns skip=true
// when the worker should exit early without error.
func prepareCheckpoint(ctx context.Context, in WorkerInput) (prepareResult, error) {
	semDir := filepath.Join(in.RepoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if !util.IsEnabled(semDir) {
		return prepareResult{skip: true}, nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 5 * time.Second,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return prepareResult{}, fmt.Errorf("open db: %w", err)
	}

	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		_ = sqlstore.Close(h)
		if errors.Is(err, sql.ErrNoRows) {
			wlog("worker: checkpoint %s not found, skipping\n", in.CheckpointID)
			return prepareResult{skip: true}, nil
		}
		return prepareResult{}, fmt.Errorf("get checkpoint: %w", err)
	}
	switch cp.Status {
	case "complete":
		_ = sqlstore.Close(h)
		wlog("worker: checkpoint %s already complete, skipping\n", in.CheckpointID)
		return prepareResult{skip: true}, nil
	case "failed":
		_ = sqlstore.Close(h)
		wlog("worker: checkpoint %s marked failed, skipping\n", in.CheckpointID)
		return prepareResult{skip: true}, nil
	}

	// Setup failures are transient (a blob dir or repo can recover);
	// the claim layer schedules the retry or records terminal failure.
	blobStore, err := blobs.NewStore(objectsDir)
	if err != nil {
		_ = sqlstore.Close(h)
		return prepareResult{}, fmt.Errorf("init blob store: %w", err)
	}

	repo, err := git.OpenRepo(in.RepoRoot)
	if err != nil {
		_ = sqlstore.Close(h)
		return prepareResult{}, fmt.Errorf("open repo: %w", err)
	}

	return prepareResult{
		wctx: &workerContext{
			h:         h,
			blobStore: blobStore,
			repo:      repo,
			cp:        cp,
			semDir:    semDir,
		},
	}, nil
}

// Run serializes and drains commit-linked work for one repository.
// The requested checkpoint is a wake-up signal, not a direct claim.
func (s *WorkerService) Run(ctx context.Context, in WorkerInput) error {
	semDir := filepath.Join(in.RepoRoot, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}

	lock, err := acquireRepoLock(ctx, semDir, in.CheckpointID, repoLockWait)
	if err != nil {
		if errors.Is(err, errRepoLockTimeout) {
			if checkpointSettled(ctx, in) {
				return nil
			}
			// Retryable: the marker must survive so launcher-backed
			// execution runs again. Never report success here.
			return fmt.Errorf("worker: repository busy, requested checkpoint still pending: %w", err)
		}
		return err
	}
	defer lock.release()

	// Link receipts before processing checkpoints so repository order is stable.
	if err := drainCommitReceipts(ctx, in.RepoRoot); err != nil {
		return err
	}

	// Reconcile only state protected by this repository lock.
	reconcileActiveSessions(ctx, s.registry, in.RepoRoot)

	if err := s.drainRepositoryQueue(ctx, in.RepoRoot); err != nil {
		return err
	}
	// Manual and baseline checkpoints are not part of the commit queue.
	linked, err := requestedIsCommitLinked(ctx, in)
	if err != nil {
		return err
	}
	if !linked {
		if err := s.processOne(ctx, in); err != nil {
			// Non-commit checkpoints are outside the retry queue.
			failRequestedCheckpoint(ctx, in, err)
			return err
		}
	}
	return s.drainRepositoryQueue(ctx, in.RepoRoot)
}

// requestedIsCommitLinked reports whether the requested checkpoint has
// a commit link, in which case the ordered drain owns it.
func requestedIsCommitLinked(ctx context.Context, in WorkerInput) (bool, error) {
	if in.CheckpointID == "" {
		return false, nil
	}
	dbPath := filepath.Join(in.RepoRoot, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return false, fmt.Errorf("open db for commit-link check: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	links, err := h.Queries.GetCommitLinksByCheckpoint(ctx, in.CheckpointID)
	if err != nil {
		return false, fmt.Errorf("commit-link check: %w", err)
	}
	return len(links) > 0, nil
}

// checkpointSettled reports whether the requested checkpoint no longer
// needs this worker (complete, failed, missing, or untracked repo).
func checkpointSettled(ctx context.Context, in WorkerInput) bool {
	semDir := filepath.Join(in.RepoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return false
	}
	defer func() { _ = sqlstore.Close(h) }()
	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		return errors.Is(err, sql.ErrNoRows)
	}
	return cp.Status != "pending"
}

// drainRepositoryQueue processes commit-linked checkpoints in sequence.
// Each checkpoint is claimed with a durable lease. A failed, leased, or
// retry-delayed queue head prevents later checkpoints from running.
func (s *WorkerService) drainRepositoryQueue(ctx context.Context, repoRoot string) error {
	const maxClaimRaces = 10
	claimRaces := 0
	for {
		queue, maxCompleteSeq, err := listPendingCommitLinked(ctx, repoRoot)
		if err != nil {
			return err
		}
		if len(queue) == 0 {
			return nil
		}
		now := time.Now().UnixMilli()
		progressed := false
		raced := false
		for _, item := range queue {
			switch item.Status {
			case "failed":
				if item.Sequence < maxCompleteSeq {
					// Already passed by a completed successor.
					continue
				}
				wlog("worker: queue blocked by terminally failed checkpoint %s (sequence %d, attempts %d): %s; "+
					"run `semantica worker retry %s` after addressing the cause\n",
					item.CheckpointID, item.Sequence, item.AttemptCount, item.LastError, item.CheckpointID)
				return nil
			case "pending":
				if item.LeaseUntil > now {
					// A live lease on a pending row is the processing
					// state. Honor it until expiry even though the
					// repository lock says the holder is gone.
					return &ErrLeaseHeld{CheckpointID: item.CheckpointID, Until: time.UnixMilli(item.LeaseUntil)}
				}
				if item.NextAttemptAt > now {
					return &ErrRetryScheduled{CheckpointID: item.CheckpointID, At: time.UnixMilli(item.NextAttemptAt)}
				}
			}
			claimed, err := s.claimAndProcess(ctx, repoRoot, item)
			if err != nil {
				return err
			}
			if !claimed {
				// The claim raced away; re-read the queue.
				raced = true
				break
			}
			progressed = true
		}
		if raced {
			claimRaces++
			if claimRaces >= maxClaimRaces {
				// Preserve the marker while runnable work may remain.
				return fmt.Errorf("worker: claim raced %d times for repository %s; retrying on a later drain", claimRaces, repoRoot)
			}
			continue
		}
		if !progressed {
			return nil
		}
	}
}

type queueItem struct {
	CheckpointID  string
	CommitHash    string
	Status        string
	Sequence      int64
	AttemptCount  int64
	LastError     string
	NextAttemptAt int64
	LeaseUntil    int64
}

// workerProcess is a seam for drain-order tests.
var workerProcess = (*WorkerService).processOne

func listPendingCommitLinked(ctx context.Context, repoRoot string) ([]queueItem, int64, error) {
	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, 0, fmt.Errorf("open db for queue: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("resolve repository: %w", err)
	}
	rows, err := h.Queries.ListPendingCommitLinkedCheckpoints(ctx, repo.RepositoryID)
	if err != nil {
		return nil, 0, fmt.Errorf("list pending queue: %w", err)
	}
	var maxCompleteSeq int64
	if latest, err := h.Queries.GetMostRecentCommitLinkedCheckpoint(ctx, repo.RepositoryID); err == nil {
		maxCompleteSeq = latest.RepositorySequence
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("resolve completed boundary: %w", err)
	}
	var queue []queueItem
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.CheckpointID] {
			continue
		}
		seen[r.CheckpointID] = true
		queue = append(queue, queueItem{
			CheckpointID:  r.CheckpointID,
			CommitHash:    r.CommitHash,
			Status:        r.Status,
			Sequence:      r.RepositorySequence,
			AttemptCount:  r.AttemptCount,
			LastError:     r.LastError.String,
			NextAttemptAt: r.NextAttemptAt,
			LeaseUntil:    r.LeaseUntil.Int64,
		})
	}
	return queue, maxCompleteSeq, nil
}

// processOne runs the full enrichment pipeline for one checkpoint.
// Callers must hold the repository worker lock.
func (s *WorkerService) processOne(ctx context.Context, in WorkerInput) error {
	prep, err := prepareCheckpoint(ctx, in)
	if err != nil {
		return err
	}
	if prep.skip {
		return nil
	}
	wctx := prep.wctx
	defer wctx.close()

	// Build the manifest, link sessions, update stats, and compute AI%.
	er, err := enrichCheckpoint(ctx, wctx, in)
	if err != nil {
		return err
	}

	// Mark the checkpoint complete only after enrichment is written.
	if err := wctx.h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: er.manifestHash, Valid: true},
		SizeBytes:    sql.NullInt64{Int64: er.totalBytes, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID: in.CheckpointID,
	}); err != nil {
		return fmt.Errorf("complete checkpoint: %w", err)
	}

	wlog("worker: checkpoint %s complete (%d files, %d changed, %d bytes, commit %s)\n",
		in.CheckpointID, er.fileCount, er.filesChanged, er.totalBytes, in.CommitHash)

	// Run post-completion side effects. Errors are logged and do not fail
	// the worker after checkpoint completion.
	runPostCompletion(ctx, wctx, in)

	return nil
}

// runPostCompletion runs all best-effort side effects after the checkpoint
// has been marked complete. Errors are logged, not propagated.
func runPostCompletion(ctx context.Context, wctx *workerContext, in WorkerInput) {
	if in.CommitHash != "" && util.IsPlaybookEnabled(wctx.semDir) {
		spawnAutoPlaybook(wctx.semDir, in.CheckpointID, in.CommitHash, in.RepoRoot)
	}

	// Drain all packaged manifests. The manifest timestamp is set during
	// packaging, so checkpoint timestamps are too early for this filter.
	_ = drainAllPackagedProvenance(ctx, wctx.semDir, in.RepoRoot)

	livePushRetried := false
	if in.CommitHash != "" && util.IsConnected(wctx.semDir) {
		pr := pushAttribution(ctx, wctx.repo, wctx.h, in.CommitHash, in.CheckpointID)
		if pr.Action == PushRetry {
			livePushRetried = true
			handlePushRetryBackfill(ctx, wctx, in.CommitHash)
		}
	}

	if util.IsConnected(wctx.semDir) && !livePushRetried {
		drainBackfillFromWorker(ctx, in.RepoRoot, wctx.semDir)
	}
}

// failRequestedCheckpoint records a terminal failure for a directly
// requested non-commit checkpoint, which is outside the retry queue.
func failRequestedCheckpoint(ctx context.Context, in WorkerInput, cause error) {
	dbPath := filepath.Join(in.RepoRoot, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		wlog("worker: record failure for %s: %v\n", in.CheckpointID, err)
		return
	}
	defer func() { _ = sqlstore.Close(h) }()
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		LastError:    sqlstore.NullStr(cause.Error()),
		CheckpointID: in.CheckpointID,
	}); err != nil {
		wlog("worker: fail checkpoint %s: %v\n", in.CheckpointID, err)
	}
}
