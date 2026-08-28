package service

import (
	"context"
	"database/sql"
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

	// Disabled repositories are a no-op.
	semDir := filepath.Join(repoRoot, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}

	// Capture the tree and parent used to bind the handoff to the commit.
	tree, err := repo.StagedTree(ctx)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: resolve staged tree failed: %v", err)
		return nil
	}
	head, err := repo.HeadOrEmpty(ctx)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: resolve HEAD failed: %v", err)
		return nil
	}

	checkpointID := uuid.NewString()
	now := time.Now().UnixMilli()
	baseHandoff := commitHandoff{CheckpointID: checkpointID, CreatedAt: now, Tree: tree, Head: head}

	// Persist the handoff before SQLite so database failures remain recoverable.
	if err := writeCommitHandoff(semDir, baseHandoff); err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: write handoff failed: %v", err)
		return nil
	}

	// Pending receipts must be sequenced before this checkpoint.
	if pending, err := commitReceiptsPending(semDir); err != nil || pending {
		if err != nil {
			util.AppendActivityLog(semDir, "pre-commit warning: list receipts failed: %v", err)
		}
		return nil
	}

	// Fast path: create the pending checkpoint before the commit completes.
	dbPath := filepath.Join(semDir, "lineage.db")
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

	// Optionally pair a bounded workspace freeze with the commit checkpoint.
	// Failures fall back to the existing commit-only path.
	if util.WorkspaceFreezeEnabled(semDir) &&
		s.tryPairWorkspaceObservation(ctx, h, repoID, repoRoot, semDir, baseHandoff, checkpointID, now) {
		return nil
	}

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: checkpointID,
		RepositoryID: repoID,
		CreatedAt:    now,
		Kind:         string(CheckpointAuto),
		Trigger:      sql.NullString{String: "commit", Valid: true},
		Message:      sql.NullString{String: "Auto checkpoint", Valid: true},
		ManifestHash: sql.NullString{}, // NULL - filled by worker
		SizeBytes:    sql.NullInt64{},  // NULL - filled by worker
		Status:       "pending",
		CompletedAt:  sql.NullInt64{}, // NULL - filled by worker
	}); err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: insert checkpoint failed: %v", err)
		return nil
	}

	return nil
}
