package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

const maxBackfillRetryAttempts = 3

// BackfillResult summarizes one run of the attribution backfill loop.
type BackfillResult struct {
	Uploaded int
	Skipped  int
	Failed   bool   // true if the batch stopped on a retryable error
	Reason   string // human-readable reason for failure
	Done     bool   // true if backfill is now complete
}

// InitBackfillState snapshots the latest commit link as the replay cutoff
// and upserts the backfill row. If no commit links exist, returns false.
func InitBackfillState(ctx context.Context, h *sqlstore.Handle, connectedRepoID, repositoryID string) (bool, error) {
	latest, err := h.Queries.GetLatestCommitLink(ctx, repositoryID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("get latest commit link: %w", err)
	}

	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID:  connectedRepoID,
		RepositoryID:     repositoryID,
		CutoffLinkedAt:   latest.LinkedAt,
		CutoffCommitHash: latest.CommitHash,
		UpdatedAt:        time.Now().UnixMilli(),
	}); err != nil {
		return false, fmt.Errorf("upsert backfill state: %w", err)
	}
	return true, nil
}

// DrainBackfillBatch runs up to `limit` replay pushes for the given backfill.
// It opens the DB, loads backfill state, iterates candidates, and updates
// cursor/failure state as it goes. Safe to call from connect or worker.
func DrainBackfillBatch(ctx context.Context, repoRoot, connectedRepoID string, limit int) BackfillResult {
	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return BackfillResult{Failed: true, Reason: fmt.Sprintf("open db: %v", err)}
	}
	defer func() { _ = sqlstore.Close(h) }()

	bf, err := h.Queries.GetAttributionBackfill(ctx, connectedRepoID)
	if err != nil {
		if err == sql.ErrNoRows {
			return BackfillResult{Done: true}
		}
		return BackfillResult{Failed: true, Reason: fmt.Sprintf("load backfill state: %v", err)}
	}
	if bf.Status == "complete" {
		return BackfillResult{Done: true}
	}

	repo, err := git.OpenRepo(repoRoot)
	if err != nil {
		return BackfillResult{Failed: true, Reason: fmt.Sprintf("open repo: %v", err)}
	}

	candidates, err := h.Queries.ListBackfillReplayCandidates(ctx, sqldb.ListBackfillReplayCandidatesParams{
		RepositoryID:     bf.RepositoryID,
		CursorLinkedAt:   bf.CursorLinkedAt,
		CursorCommitHash: bf.CursorCommitHash,
		CutoffLinkedAt:   bf.CutoffLinkedAt,
		CutoffCommitHash: bf.CutoffCommitHash,
		BatchLimit:       int64(limit),
	})
	if err != nil {
		return BackfillResult{Failed: true, Reason: fmt.Sprintf("list candidates: %v", err)}
	}

	if len(candidates) == 0 {
		if err := h.Queries.CompleteBackfill(ctx, sqldb.CompleteBackfillParams{
			UpdatedAt:       time.Now().UnixMilli(),
			ConnectedRepoID: connectedRepoID,
		}); err != nil {
			return BackfillResult{Failed: true, Reason: fmt.Sprintf("complete backfill: %v", err)}
		}
		return BackfillResult{Done: true}
	}

	var result BackfillResult
	now := time.Now().UnixMilli()

	for _, c := range candidates {
		// If this commit is the one that previously failed, check retry cap.
		if bf.FailedCommitHash.Valid && bf.FailedCommitHash.String == c.CommitHash {
			if bf.RetryAttempts >= maxBackfillRetryAttempts {
				// Retry cap reached - skip this commit.
				if err := h.Queries.AdvanceBackfillCursor(ctx, sqldb.AdvanceBackfillCursorParams{
					CursorLinkedAt:   c.LinkedAt,
					CursorCommitHash: c.CommitHash,
					UpdatedAt:        now,
					ConnectedRepoID:  connectedRepoID,
				}); err != nil {
					result.Failed = true
					result.Reason = fmt.Sprintf("advance backfill cursor: %v", err)
					return result
				}
				result.Skipped++
				continue
			}
		}

		pr := tryPushAttribution(ctx, repo, h, c.CommitHash, c.CheckpointID)

		switch pr.Action {
		case PushUploaded:
			if err := h.Queries.AdvanceBackfillCursor(ctx, sqldb.AdvanceBackfillCursorParams{
				CursorLinkedAt:   c.LinkedAt,
				CursorCommitHash: c.CommitHash,
				UpdatedAt:        now,
				ConnectedRepoID:  connectedRepoID,
			}); err != nil {
				result.Failed = true
				result.Reason = fmt.Sprintf("advance backfill cursor: %v", err)
				return result
			}
			result.Uploaded++

		case PushSkip:
			if err := h.Queries.AdvanceBackfillCursor(ctx, sqldb.AdvanceBackfillCursorParams{
				CursorLinkedAt:   c.LinkedAt,
				CursorCommitHash: c.CommitHash,
				UpdatedAt:        now,
				ConnectedRepoID:  connectedRepoID,
			}); err != nil {
				result.Failed = true
				result.Reason = fmt.Sprintf("advance backfill cursor: %v", err)
				return result
			}
			result.Skipped++

		case PushRetry:
			if err := h.Queries.RecordBackfillFailure(ctx, sqldb.RecordBackfillFailureParams{
				FailedCommitHash: sql.NullString{String: c.CommitHash, Valid: true},
				LastError:        sql.NullString{String: pr.Err.Error(), Valid: true},
				UpdatedAt:        now,
				ConnectedRepoID:  connectedRepoID,
			}); err != nil {
				result.Failed = true
				result.Reason = fmt.Sprintf("record backfill failure: %v", err)
				return result
			}
			result.Failed = true
			result.Reason = pr.Err.Error()
			return result
		}
	}

	// Check if we've drained everything.
	remaining, err := h.Queries.ListBackfillReplayCandidates(ctx, sqldb.ListBackfillReplayCandidatesParams{
		RepositoryID:     bf.RepositoryID,
		CursorLinkedAt:   candidates[len(candidates)-1].LinkedAt,
		CursorCommitHash: candidates[len(candidates)-1].CommitHash,
		CutoffLinkedAt:   bf.CutoffLinkedAt,
		CutoffCommitHash: bf.CutoffCommitHash,
		BatchLimit:       1,
	})
	if err != nil {
		result.Failed = true
		result.Reason = fmt.Sprintf("list remaining backfill candidates: %v", err)
		return result
	}
	if len(remaining) == 0 {
		if err := h.Queries.CompleteBackfill(ctx, sqldb.CompleteBackfillParams{
			UpdatedAt:       time.Now().UnixMilli(),
			ConnectedRepoID: connectedRepoID,
		}); err != nil {
			result.Failed = true
			result.Reason = fmt.Sprintf("complete backfill: %v", err)
			return result
		}
		result.Done = true
	}

	return result
}

// ExtendBackfillCutoff extends the backfill cutoff to include a commit that
// failed during live push. Re-opens completed backfills if needed.
func ExtendBackfillCutoff(ctx context.Context, h *sqlstore.Handle, connectedRepoID, repositoryID, commitHash string, linkedAt int64) error {
	return h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID:  connectedRepoID,
		RepositoryID:     repositoryID,
		CutoffLinkedAt:   linkedAt,
		CutoffCommitHash: commitHash,
		UpdatedAt:        time.Now().UnixMilli(),
	})
}
