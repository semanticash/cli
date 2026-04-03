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
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type PreCommitService struct{}

func NewPreCommitService() *PreCommitService { return &PreCommitService{} }

func (s *PreCommitService) HandlePreCommit(ctx context.Context, repoPath string) error {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return err
	}
	repoRoot := repo.Root()

	// If Semantica isn't enabled, no-op (don't block commit).
	semDir := filepath.Join(repoRoot, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}
	dbPath := filepath.Join(semDir, "lineage.db")

	// Open DB with a short timeout - never block commit.
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 50 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: open db failed: %v", err)
		return nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID, err := sqlstore.EnsureRepository(ctx, h.Queries, repoRoot)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: ensure repo failed: %v", err)
		return nil
	}

	// Insert a pending checkpoint stub (no manifest, no blobs - worker fills those in).
	checkpointID := uuid.NewString()
	now := time.Now().UnixMilli()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: checkpointID,
		RepositoryID: repoID,
		CreatedAt:    now,
		Kind:         string(CheckpointAuto),
		Trigger:      sql.NullString{String: "commit", Valid: true},
		Message:      sql.NullString{String: "Auto checkpoint", Valid: true},
		ManifestHash: sql.NullString{}, // NULL - filled by worker
		SizeBytes:    sql.NullInt64{},   // NULL - filled by worker
		Status:       "pending",
		CompletedAt:  sql.NullInt64{}, // NULL - filled by worker
	}); err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: insert checkpoint failed: %v", err)
		return nil
	}

	// Write handoff file: checkpoint_id|created_at
	handoffPath := util.PreCommitCheckpointPath(semDir)
	payload := fmt.Sprintf("%s|%d\n", checkpointID, now)

	tmp := handoffPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(payload), 0o644); err != nil {
		return nil
	}
	_ = os.Remove(handoffPath)
	if err := os.Rename(tmp, handoffPath); err != nil {
		_ = os.Remove(tmp)
		return nil
	}

	return nil
}
