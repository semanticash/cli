package service

import (
	"context"
	"fmt"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// enrichResult carries the outputs of checkpoint enrichment to completion
// and post-completion work.
type enrichResult struct {
	manifestHash string
	totalBytes   int64
	filesChanged int64
	fileCount    int
	sessionCount int
}

// workerWindows holds the two different "previous checkpoint" boundaries
// used by the worker. The worker uses:
//   - previous completed checkpoint for session linking and file counting
//   - previous commit-linked checkpoint for the attribution event window
//
// These serve different purposes and must not be conflated.
type workerWindows struct {
	// sessionAfterTs is the lower bound for session linking and file counting.
	sessionAfterTs int64

	// attrAfterTs is the lower bound for the attribution event window.
	attrAfterTs int64

	// prevCommitLinked is the previous commit-linked checkpoint, if any.
	// Passed to attributeWithCarryForward for historical lookback.
	prevCommitLinked *sqldb.Checkpoint
}

// resolveWorkerWindows looks up both previous-checkpoint boundaries.
func resolveWorkerWindows(ctx context.Context, h *sqlstore.Handle, cp sqldb.Checkpoint) workerWindows {
	var w workerWindows

	prev, err := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if err == nil {
		w.sessionAfterTs = prev.CreatedAt
	}

	prevCL, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if err == nil {
		w.attrAfterTs = prevCL.CreatedAt
		cp := prevCL // copy to avoid aliasing loop variable
		w.prevCommitLinked = &cp
	}

	return w
}

// linkSessionsToCheckpoint finds sessions with events in the window and
// inserts session_checkpoint rows. Returns the set of linked session IDs.
func linkSessionsToCheckpoint(ctx context.Context, h *sqlstore.Handle, checkpointID string, cp sqldb.Checkpoint, afterTs int64) map[string]bool {
	windowSessions, err := h.Queries.ListSessionsWithEventsInWindow(ctx, sqldb.ListSessionsWithEventsInWindowParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if err != nil {
		wlog("worker: list sessions in window: %v\n", err)
	}

	seen := make(map[string]bool, len(windowSessions))
	for _, sid := range windowSessions {
		seen[sid] = true
	}

	for sid := range seen {
		if err := h.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
			SessionID:    sid,
			CheckpointID: checkpointID,
		}); err != nil {
			wlog("worker: link session %s to checkpoint: %v\n", sid, err)
		}
	}

	return seen
}

// enrichCheckpoint builds the manifest, links sessions, attaches the commit
// to its implementation, computes stats, and computes AI percentage. All
// required enrichment must finish before the checkpoint is marked complete.
func enrichCheckpoint(ctx context.Context, wctx *workerContext, in WorkerInput) (enrichResult, error) {
	h := wctx.h
	repo := wctx.repo
	blobStore := wctx.blobStore
	cp := wctx.cp

	paths, err := repo.ListFilesFromGit(ctx)
	if err != nil {
		return enrichResult{}, fmt.Errorf("list files: %w", err)
	}

	// Manifest.
	prevManifest := loadPreviousManifest(ctx, h, blobStore, cp.RepositoryID, cp.CreatedAt)
	mr, err := blobs.BuildManifest(ctx, blobStore, in.RepoRoot, paths, repo.ReadFile, prevManifest.files)
	if err != nil {
		return enrichResult{}, err
	}

	// Windows.
	windows := resolveWorkerWindows(ctx, h, cp)

	// Session linking (must happen before implementation attach).
	seen := linkSessionsToCheckpoint(ctx, h, in.CheckpointID, cp, windows.sessionAfterTs)

	// Implementation attach (depends on session_checkpoints written above).
	if in.CommitHash != "" {
		handleImplementationPostCommit(ctx, wctx.semDir, in.RepoRoot, in.CommitHash)
	}

	// Stats.
	filesChanged := countChangedFiles(prevManifest, mr.Manifest.Files)
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: in.CheckpointID,
		SessionCount: int64(len(seen)),
		FilesChanged: filesChanged,
	}); err != nil {
		wlog("worker: upsert checkpoint stats: %v\n", err)
	}

	// AI attribution.
	if in.CommitHash != "" {
		computeEnrichmentAttribution(ctx, wctx, in, windows)
	}

	return enrichResult{
		manifestHash: mr.ManifestHash,
		totalBytes:   mr.TotalBytes,
		filesChanged: filesChanged,
		fileCount:    len(paths),
		sessionCount: len(seen),
	}, nil
}

// computeEnrichmentAttribution diffs the commit, runs carry-forward attribution,
// and writes the AI percentage to the checkpoint. Best-effort: failures are
// logged, not propagated to the caller.
func computeEnrichmentAttribution(ctx context.Context, wctx *workerContext, in WorkerInput, windows workerWindows) {
	diffBytes, err := wctx.repo.DiffForCommit(ctx, in.CommitHash)
	if err != nil || len(diffBytes) == 0 {
		return
	}

	cfr, err := attributeWithCarryForward(ctx, wctx.h, wctx.blobStore, diffBytes, ComputeAIPercentInput{
		RepoRoot: in.RepoRoot,
		RepoID:   wctx.cp.RepositoryID,
		AfterTs:  windows.attrAfterTs,
		UpToTs:   wctx.cp.CreatedAt,
	}, windows.prevCommitLinked, wctx.semDir)
	if err != nil {
		return
	}

	if err := wctx.h.Queries.UpdateCheckpointAIPercentage(ctx, sqldb.UpdateCheckpointAIPercentageParams{
		AiPercentage: cfr.result.Percent,
		CheckpointID: in.CheckpointID,
	}); err != nil {
		wlog("worker: update AI percentage: %v\n", err)
	}
	wlog("worker: AI attribution: %.0f%%\n", cfr.result.Percent)
}
