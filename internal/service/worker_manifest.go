package service

import (
	"context"
	"database/sql"
	"errors"
	"runtime"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// prevManifestResult holds the result of loading the previous manifest.
type prevManifestResult struct {
	files        []blobs.ManifestFile
	checkpointID string
	exists       bool
	ok           bool
}

// loadPreviousManifest returns the previous completed manifest when
// available. The predecessor is selected by repository_sequence.
func loadPreviousManifest(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, repoID string, cpSequence int64) prevManifestResult {
	prev, err := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID:       repoID,
		RepositorySequence: cpSequence,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return prevManifestResult{}
		}
		wlog("worker: get previous checkpoint: %v\n", err)
		return prevManifestResult{exists: true}
	}

	if !prev.ManifestHash.Valid || prev.ManifestHash.String == "" {
		return prevManifestResult{checkpointID: prev.CheckpointID, exists: true}
	}

	rawManifest, err := bs.Get(ctx, prev.ManifestHash.String)
	if err != nil {
		wlog("worker: load previous manifest: %v\n", err)
		return prevManifestResult{checkpointID: prev.CheckpointID, exists: true}
	}

	prevManifest, err := blobs.ParseManifest(rawManifest)
	if err != nil {
		wlog("worker: parse previous manifest: %v\n", err)
		return prevManifestResult{checkpointID: prev.CheckpointID, exists: true}
	}

	return prevManifestResult{files: prevManifest.Files, checkpointID: prev.CheckpointID, exists: true, ok: true}
}

// reusableCommitRangeFiles returns previous manifest entries eligible for
// reuse by a commit-linked checkpoint. Both checkpoints must have one
// unambiguous commit link. Eligible paths must be unchanged between commits,
// tracked normally in the current index, and unchanged in the worktree.
// BuildManifest also applies its metadata check.
//
// Windows and unverifiable states return no reusable entries. Context errors
// propagate; other failures conservatively rehash every file.
func reusableCommitRangeFiles(ctx context.Context, h *sqlstore.Handle, repo *git.Repo, prev prevManifestResult, curCheckpointID, curCommit string) ([]blobs.ManifestFile, error) {
	// Preserve cancellation while treating other failures as reuse misses.
	failClosed := func(format string, args ...any) ([]blobs.ManifestFile, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if format != "" {
			wlog("worker: "+format+"\n", args...)
		}
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Git for Windows cannot reliably distinguish an in-place rewrite with
	// restored size and mtime, so it cannot safely reuse manifest entries.
	if runtime.GOOS == "windows" {
		return failClosed("")
	}

	if !prev.ok || prev.checkpointID == "" || curCheckpointID == "" || !git.IsFullCommitHash(curCommit) {
		return failClosed("")
	}

	prevLinks, err := h.Queries.GetCommitLinksByCheckpoint(ctx, prev.checkpointID)
	if err != nil {
		return failClosed("previous checkpoint commit links: %v", err)
	}
	if len(prevLinks) != 1 {
		// Zero links means no boundary; multiple make it ambiguous.
		return failClosed("")
	}
	prevCommit := prevLinks[0].CommitHash
	if !git.IsFullCommitHash(prevCommit) {
		return failClosed("")
	}
	curLinks, err := h.Queries.GetCommitLinksByCheckpoint(ctx, curCheckpointID)
	if err != nil {
		return failClosed("current checkpoint commit links: %v", err)
	}
	if len(curLinks) != 1 || curLinks[0].CommitHash != curCommit {
		return failClosed("")
	}

	changed, err := repo.ChangedPathsBetween(ctx, prevCommit, curCommit)
	if err != nil {
		return failClosed("changed paths %s..%s: %v", prevCommit, curCommit, err)
	}
	atCommit, err := repo.TrackedFilesAtCommit(ctx, curCommit)
	if err != nil {
		return failClosed("tracked files at %s: %v", curCommit, err)
	}
	inIndex, err := repo.OrdinaryTrackedFiles(ctx)
	if err != nil {
		return failClosed("ordinary tracked files: %v", err)
	}
	drifted, err := repo.ChangedPathsFromCommitToWorktree(ctx, curCommit)
	if err != nil {
		return failClosed("worktree drift from %s: %v", curCommit, err)
	}
	// The final Git command may have consumed the deadline.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	excluded := make(map[string]bool, len(changed)+len(drifted))
	for _, p := range changed {
		excluded[p] = true
	}
	for _, p := range drifted {
		excluded[p] = true
	}
	indexSet := make(map[string]bool, len(inIndex))
	for _, p := range inIndex {
		indexSet[p] = true
	}
	reusable := make(map[string]bool, len(atCommit))
	for _, p := range atCommit {
		if indexSet[p] && !excluded[p] {
			reusable[p] = true
		}
	}

	eligible := make([]blobs.ManifestFile, 0, len(prev.files))
	for _, f := range prev.files {
		if reusable[f.Path] {
			eligible = append(eligible, f)
		}
	}
	return eligible, nil
}

// countChangedFiles compares current files to the previous manifest when one
// is available.
func countChangedFiles(prev prevManifestResult, currentFiles []blobs.ManifestFile) int64 {
	if !prev.exists {
		return int64(len(currentFiles))
	}
	if !prev.ok {
		return 0
	}

	prevIndex := make(map[string]string, len(prev.files))
	for _, f := range prev.files {
		prevIndex[f.Path] = f.Blob
	}

	var changed int64
	for _, f := range currentFiles {
		if prevBlob, ok := prevIndex[f.Path]; !ok || prevBlob != f.Blob {
			changed++
		}
		delete(prevIndex, f.Path)
	}
	// Whatever remains in prevIndex are deleted files.
	changed += int64(len(prevIndex))

	return changed
}
