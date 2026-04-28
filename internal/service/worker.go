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
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/copilot"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
)

type WorkerService struct{}

func NewWorkerService() *WorkerService { return &WorkerService{} }

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

	blobStore, err := blobs.NewStore(objectsDir)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		_ = sqlstore.Close(h)
		return prepareResult{}, fmt.Errorf("init blob store: %w", err)
	}

	repo, err := git.OpenRepo(in.RepoRoot)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
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

func (s *WorkerService) Run(ctx context.Context, in WorkerInput) error {
	prep, err := prepareCheckpoint(ctx, in)
	if err != nil {
		return err
	}
	if prep.skip {
		return nil
	}
	wctx := prep.wctx
	defer wctx.close()

	// Reconciliation must run before manifest/checkpoint completion so
	// recovered events are included in this checkpoint.
	reconcileActiveSessions(ctx)

	// Process pending implementation observations. Errors are logged so the
	// worker can continue with checkpoint enrichment.
	reconcileImplementations(ctx, in.RepoRoot)

	// Build the manifest, link sessions, update stats, and compute AI%.
	er, err := enrichCheckpoint(ctx, wctx, in)
	if err != nil {
		failCheckpoint(ctx, wctx.h, in.CheckpointID)
		return err
	}

	// Mark the checkpoint complete only after enrichment is written.
	if err := wctx.h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: er.manifestHash, Valid: true},
		SizeBytes:    sql.NullInt64{Int64: er.totalBytes, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID: in.CheckpointID,
	}); err != nil {
		failCheckpoint(ctx, wctx.h, in.CheckpointID)
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

	if util.IsConnected(wctx.semDir) {
		syncProvenance(ctx, in.RepoRoot, wctx.cp.CreatedAt)
	}

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

func failCheckpoint(ctx context.Context, h *sqlstore.Handle, checkpointID string) {
	if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID: checkpointID,
	}); err != nil {
		wlog("worker: fail checkpoint %s: %v\n", checkpointID, err)
	}
}
