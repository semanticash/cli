package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
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
	// sessionWindow bounds session linking and file counting
	// (previous completed checkpoint -> this checkpoint).
	sessionWindow eventWindow

	// attrWindow bounds the attribution event window (previous
	// commit-linked checkpoint -> this checkpoint).
	attrWindow eventWindow

	// prevCommitLinked is the previous commit-linked checkpoint, if any.
	// Passed to attributeWithCarryForward for historical lookback.
	prevCommitLinked *sqldb.Checkpoint
}

// resolveWorkerWindows looks up both previous-checkpoint boundaries.
func resolveWorkerWindows(ctx context.Context, h *sqlstore.Handle, cp sqldb.Checkpoint) workerWindows {
	w := workerWindows{
		sessionWindow: windowBetween(nil, cp),
		attrWindow:    windowBetween(nil, cp),
	}

	prev, err := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID:       cp.RepositoryID,
		RepositorySequence: cp.RepositorySequence,
	})
	if err == nil {
		w.sessionWindow = windowBetween(&prev, cp)
	}

	prevCL, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID:       cp.RepositoryID,
		RepositorySequence: cp.RepositorySequence,
	})
	if err == nil {
		w.attrWindow = windowBetween(&prevCL, cp)
		cp := prevCL // copy to avoid aliasing loop variable
		w.prevCommitLinked = &cp
	}

	return w
}

// linkSessionsToCheckpoint finds sessions with events in the window and
// inserts session_checkpoint rows. Returns the set of linked session IDs.
func linkSessionsToCheckpoint(ctx context.Context, h *sqlstore.Handle, checkpointID string, cp sqldb.Checkpoint, win eventWindow) map[string]bool {
	windowSessions, err := h.Queries.ListSessionsWithEventsInWindow(ctx, sqldb.ListSessionsWithEventsInWindowParams{
		RepositoryID: cp.RepositoryID,
		UseCursor:    win.cursorFlag(),
		AfterCursor:  win.cursorAfter(),
		UpToCursor:   win.cursorUpTo(),
		AfterTs:      win.afterTs,
		UpToTs:       win.upToTs,
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

// enrichCheckpoint builds the manifest, links sessions, computes stats, and
// computes AI percentage. Required enrichment must finish before the checkpoint
// is marked complete.
func enrichCheckpoint(ctx context.Context, wctx *workerContext, in WorkerInput) (enrichResult, error) {
	h := wctx.h
	repo := wctx.repo
	blobStore := wctx.blobStore
	cp := wctx.cp

	paths, err := repo.ListFilesFromGit(ctx)
	if err != nil {
		return enrichResult{}, fmt.Errorf("list files: %w", err)
	}

	// Commit-linked checkpoints reuse only files Git verifies as unchanged
	// across the commit range and current worktree.
	prevManifest := loadPreviousManifest(ctx, h, blobStore, cp.RepositoryID, cp.RepositorySequence)
	reusableFiles := prevManifest.files
	if in.CommitHash != "" {
		reusableFiles, err = reusableCommitRangeFiles(ctx, h, repo, prevManifest, in.CheckpointID, in.CommitHash)
		if err != nil {
			return enrichResult{}, fmt.Errorf("reusable manifest files: %w", err)
		}
	}
	mr, err := blobs.BuildManifest(ctx, blobStore, in.RepoRoot, paths, repo.ReadFile, reusableFiles)
	if err != nil {
		return enrichResult{}, err
	}

	// Windows.
	windows := resolveWorkerWindows(ctx, h, cp)

	// Session linking.
	seen := linkSessionsToCheckpoint(ctx, h, in.CheckpointID, cp, windows.sessionWindow)

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
	if err != nil {
		return
	}
	if len(diffBytes) == 0 {
		// An empty diff is a completed attribution result.
		markAttributionComputed(ctx, wctx.h, in.CheckpointID)
		return
	}

	// Use the repository's scoring version for persisted attribution.
	cfr, err := attributeWithCarryForward(ctx, wctx.h, wctx.blobStore, diffBytes, ComputeAIPercentInput{
		RepoRoot: in.RepoRoot,
		RepoID:   wctx.cp.RepositoryID,
		Window:   windows.attrWindow,
	}, windows.prevCommitLinked, wctx.semDir, util.AttributionV2Enabled(wctx.semDir))
	if errors.Is(err, ErrNoEventsInWindow) {
		// No agent evidence is a valid zero-AI result.
		markAttributionComputed(ctx, wctx.h, in.CheckpointID)
		return
	}
	if err != nil {
		return
	}

	if err := wctx.h.Queries.UpdateCheckpointAIPercentage(ctx, sqldb.UpdateCheckpointAIPercentageParams{
		AiPercentage: cfr.result.Percent,
		CheckpointID: in.CheckpointID,
	}); err != nil {
		wlog("worker: update AI percentage: %v\n", err)
		return
	}
	markAttributionComputed(ctx, wctx.h, in.CheckpointID)
	wlog("worker: AI attribution: %.0f%%\n", cfr.result.Percent)
}

// markAttributionComputed records stage completion. Write failures leave
// attribution readiness unknown.
func markAttributionComputed(ctx context.Context, h *sqlstore.Handle, checkpointID string) {
	if err := h.Queries.MarkCheckpointAttributionComputed(ctx, sqldb.MarkCheckpointAttributionComputedParams{
		AttributionComputedAt: sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID:          checkpointID,
	}); err != nil {
		wlog("worker: mark attribution computed for %s: %v\n", checkpointID, err)
	}
}
