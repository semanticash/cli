package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type CheckpointKind string

const (
	CheckpointManual   CheckpointKind = "manual"
	CheckpointAuto     CheckpointKind = "auto"
	CheckpointBaseline CheckpointKind = "baseline"
)

type CreateCheckpointInput struct {
	RepoPath string
	Kind     CheckpointKind
	Trigger  string
	Message  string
}

type CreateCheckpointResult struct {
	CheckpointID string `json:"checkpoint_id"`
	RepositoryID string `json:"repository_id"`
	ManifestHash string `json:"manifest_hash"`
	FileCount    int    `json:"file_count"`
	TotalBytes   int64  `json:"total_bytes"`
	CreatedAt    int64  `json:"created_at"`
}

type CheckpointService struct{}

func NewCheckpointService() *CheckpointService {
	return &CheckpointService{}
}

func (s *CheckpointService) Create(ctx context.Context, in CreateCheckpointInput) (*CreateCheckpointResult, error) {
	// Open repo
	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	// Ensure Semantica is enabled
	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("semantica is not enabled in this repo. run `semantica enable` first")
	}
	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	// Open database
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Ensure repo record row exists
	repoID, err := sqlstore.EnsureRepository(ctx, h.Queries, repoRoot)
	if err != nil {
		return nil, err
	}

	// List files
	paths, err := repo.ListFilesFromGit(ctx)
	if err != nil {
		return nil, err
	}

	// Store blobs + build manifest.
	// Events are already in the DB from hooks - no ingest needed.
	blobStore, err := blobs.NewStore(objectsDir)
	if err != nil {
		return nil, fmt.Errorf("init blob store: %w", err)
	}

	mr, err := blobs.BuildManifest(ctx, blobStore, repoRoot, paths, repo.ReadFile, nil)
	if err != nil {
		return nil, err
	}
	manifestHash := mr.ManifestHash
	total := mr.TotalBytes

	now := time.Now().UnixMilli()

	// Insert checkpoint row (write ordering: blobs first, then database)
	if in.Kind == "" {
		in.Kind = CheckpointManual
	}
	checkpointID := uuid.NewString()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: checkpointID,
		RepositoryID: repoID,
		CreatedAt:    now,
		Kind:         string(in.Kind),
		Trigger:      sqlstore.NullStr(in.Trigger),
		Message:      sqlstore.NullStr(in.Message),
		ManifestHash: sqlstore.NullStr(manifestHash),
		SizeBytes:    sql.NullInt64{Int64: total, Valid: true},
		Status:       "complete",
		CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("insert checkpoint: %w", err)
	}

	// Link sessions to checkpoint (INSERT OR IGNORE).
	// Events are already in the DB from hooks. Query for sessions with
	// events in the checkpoint's time window.
	var afterTs int64
	prev, prevErr := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: repoID,
		CreatedAt:    now,
	})
	if prevErr == nil {
		afterTs = prev.CreatedAt
	}

	windowSessions, sessErr := h.Queries.ListSessionsWithEventsInWindow(ctx, sqldb.ListSessionsWithEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      afterTs,
		UpToTs:       now,
	})
	if sessErr != nil {
		wlog("checkpoint: list sessions in window: %v\n", sessErr)
	}

	for _, sid := range windowSessions {
		if err := h.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
			SessionID:    sid,
			CheckpointID: checkpointID,
		}); err != nil {
			wlog("checkpoint: link session %s to checkpoint: %v\n", sid, err)
		}
	}

	return &CreateCheckpointResult{
		CheckpointID: checkpointID,
		RepositoryID: repoID,
		ManifestHash: manifestHash,
		FileCount:    len(paths),
		TotalBytes:   total,
		CreatedAt:    now,
	}, nil
}
