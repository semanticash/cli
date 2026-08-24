package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// prevManifestResult holds the result of loading the previous manifest.
type prevManifestResult struct {
	files        []blobs.ManifestFile
	manifest     *blobs.Manifest
	commitHash   string // single full commit link, when available
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

	res := prevManifestResult{
		files:        prevManifest.Files,
		manifest:     &prevManifest,
		checkpointID: prev.CheckpointID,
		exists:       true,
		ok:           true,
	}
	// Reuse requires the previous manifest to match its persisted commit link.
	if links, lerr := h.Queries.GetCommitLinksByCheckpoint(ctx, prev.CheckpointID); lerr == nil &&
		len(links) == 1 && git.IsFullCommitHash(links[0].CommitHash) {
		res.commitHash = links[0].CommitHash
	}
	return res
}

// resolveCommitAnchor derives manifest scope from persisted commit links.
// Linked checkpoints require one full hash, an auto kind, and a matching hint
// when supplied. Unlinked checkpoints reject commit hints.
func resolveCommitAnchor(ctx context.Context, h *sqlstore.Handle, cp sqldb.Checkpoint, in WorkerInput) (commit string, isCommit bool, err error) {
	links, err := h.Queries.GetCommitLinksByCheckpoint(ctx, in.CheckpointID)
	if err != nil {
		return "", false, fmt.Errorf("commit links for %s: %w", in.CheckpointID, err)
	}
	switch len(links) {
	case 0:
		if in.CommitHash != "" {
			return "", false, fmt.Errorf("worker commit %q has no persisted commit link", in.CommitHash)
		}
		return "", false, nil
	case 1:
		c := links[0].CommitHash
		if !git.IsFullCommitHash(c) {
			return "", false, fmt.Errorf("commit link %q is not a full hash", c)
		}
		if in.CommitHash != "" && in.CommitHash != c {
			return "", false, fmt.Errorf("worker commit %q does not match persisted link %q", in.CommitHash, c)
		}
		if cp.Kind != string(CheckpointAuto) {
			return "", false, fmt.Errorf("checkpoint kind %q must not carry a commit link", cp.Kind)
		}
		return c, true, nil
	default:
		return "", false, fmt.Errorf("checkpoint %s has %d commit links; expected at most one", in.CheckpointID, len(links))
	}
}

// buildCommitManifest builds a commit manifest from Git objects and reuses
// blobs from a verified predecessor.
func buildCommitManifest(ctx context.Context, repo *git.Repo, bs *blobs.Store, commit string, prev prevManifestResult) (*blobs.ManifestResult, error) {
	objectFormat, err := repo.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("object format: %w", err)
	}
	treeID, err := repo.CommitTree(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("commit tree %s: %w", commit, err)
	}
	treeEntries, err := repo.LsTreeEntries(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("ls-tree %s: %w", commit, err)
	}
	entries := make([]blobs.CommitTreeEntry, len(treeEntries))
	for i, e := range treeEntries {
		entries[i] = blobs.CommitTreeEntry{Path: e.Path, GitMode: e.Mode, GitObjectID: e.ObjectID, GitType: e.Type}
	}
	input := blobs.CommitManifestInput{
		ObjectFormat: objectFormat, CommitHash: commit, TreeID: treeID,
		Entries: entries, ReadObjects: repo.CatFileBatch,
	}
	if prev.ok && prev.manifest != nil && prev.commitHash != "" {
		input.Previous = prev.manifest
		input.PreviousCommitLink = prev.commitHash
	}
	return blobs.BuildCommitManifest(ctx, bs, input)
}

// commitFilesChanged counts the paths a commit changed against its first parent,
// with rename detection disabled (a rename counts as one deletion plus one
// addition). A root commit counts all tracked tree entries.
func commitFilesChanged(ctx context.Context, repo *git.Repo, commit string, treeEntryCount int) (int64, error) {
	parent, err := repo.FirstParentOrEmpty(ctx, commit)
	if err != nil {
		return 0, fmt.Errorf("first parent of %s: %w", commit, err)
	}
	if parent == "" {
		return int64(treeEntryCount), nil
	}
	changed, err := repo.ChangedPathsBetween(ctx, parent, commit)
	if err != nil {
		return 0, fmt.Errorf("changed paths %s..%s: %w", parent, commit, err)
	}
	return int64(len(changed)), nil
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
