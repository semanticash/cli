package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// prevManifestResult holds the result of loading the previous manifest.
type prevManifestResult struct {
	files  []blobs.ManifestFile
	exists bool
	ok     bool
}

// loadPreviousManifest returns the previous completed manifest when available.
func loadPreviousManifest(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, repoID string, cpCreatedAt int64) prevManifestResult {
	prev, err := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: repoID,
		CreatedAt:    cpCreatedAt,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return prevManifestResult{}
		}
		wlog("worker: get previous checkpoint: %v\n", err)
		return prevManifestResult{exists: true}
	}

	if !prev.ManifestHash.Valid || prev.ManifestHash.String == "" {
		return prevManifestResult{exists: true}
	}

	rawManifest, err := bs.Get(ctx, prev.ManifestHash.String)
	if err != nil {
		wlog("worker: load previous manifest: %v\n", err)
		return prevManifestResult{exists: true}
	}

	var prevManifest blobs.Manifest
	if err := json.Unmarshal(rawManifest, &prevManifest); err != nil {
		wlog("worker: unmarshal previous manifest: %v\n", err)
		return prevManifestResult{exists: true}
	}

	return prevManifestResult{files: prevManifest.Files, exists: true, ok: true}
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
