package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// PendingProvenanceInfo summarizes local turn provenance waiting to upload.
type PendingProvenanceInfo struct {
	Count                int64 `json:"count"`
	SinceLastCommitCount int64 `json:"since_last_commit_count,omitempty"`
	HasLastCommit        bool  `json:"has_last_commit"`
	LastCommitAt         int64 `json:"last_commit_at,omitempty"`
}

// PendingProvenance counts local provenance manifests that are ready or
// queued for upload. Count covers all pending local manifests; when a
// commit-linked checkpoint exists, SinceLastCommitCount covers manifests
// created after the most recent such checkpoint.
func PendingProvenance(ctx context.Context, repoRoot string) (*PendingProvenanceInfo, error) {
	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.UserFacingOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	return pendingProvenanceForRepo(ctx, h, repo.RepositoryID)
}

func pendingProvenanceForRepo(ctx context.Context, h *sqlstore.Handle, repoID string) (*PendingProvenanceInfo, error) {
	total, err := countPendingManifestsSince(ctx, h, repoID, 0)
	if err != nil {
		return nil, err
	}

	info := &PendingProvenanceInfo{Count: total}

	cp, err := h.Queries.GetMostRecentCommitLinkedCheckpoint(ctx, repoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return info, nil
		}
		return nil, err
	}

	since, err := countPendingManifestsSince(ctx, h, repoID, cp.CreatedAt+1)
	if err != nil {
		return nil, err
	}
	info.HasLastCommit = true
	info.LastCommitAt = cp.CreatedAt
	info.SinceLastCommitCount = since
	return info, nil
}

func countPendingManifestsSince(ctx context.Context, h *sqlstore.Handle, repoID string, sinceTs int64) (int64, error) {
	rows, err := h.Queries.CountManifestsByStatus(ctx, sqldb.CountManifestsByStatusParams{
		RepositoryID: repoID,
		CreatedAt:    sinceTs,
	})
	if err != nil {
		return 0, err
	}

	var count int64
	for _, row := range rows {
		switch row.Status {
		case "pending", "packaged", "uploading":
			count += row.Count
		}
	}
	return count, nil
}
