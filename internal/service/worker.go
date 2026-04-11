package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// workerContext carries the shared infrastructure handles opened during
// checkpoint preparation. It keeps the helper calls from threading
// many parameters through every call.
type workerContext struct {
	h         *sqlstore.Handle
	blobStore *blobs.Store
	repo      *git.Repo
	cp        sqldb.Checkpoint
	semDir    string
}

func (wc *workerContext) close() { _ = sqlstore.Close(wc.h) }

// prepareResult is the outcome of prepareCheckpoint.
type prepareResult struct {
	wctx *workerContext
	skip bool // true when checkpoint is already complete/failed, not found, or semantica disabled
}

func wlog(format string, args ...any) {
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s  %s", ts, msg)
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
