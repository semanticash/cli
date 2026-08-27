package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// insertCheckpointForQuery inserts a checkpoint and returns its ID and sequence.
func insertCheckpointForQuery(t *testing.T, ctx context.Context, q *sqldb.Queries, repoID, status string, manifest sql.NullString) (string, int64) {
	t.Helper()
	id := uuid.NewString()
	if err := q.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: id,
		RepositoryID: repoID,
		CreatedAt:    time.Now().UnixMilli(),
		Kind:         "auto",
		Status:       status,
		ManifestHash: manifest,
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
	cp, err := q.GetCheckpointByID(ctx, id)
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	return id, cp.RepositorySequence
}

func linkCommit(t *testing.T, ctx context.Context, q *sqldb.Queries, repoID, checkpointID, commitHash string) {
	t.Helper()
	if err := q.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: repoID,
		CheckpointID: checkpointID,
		LinkedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
}

// TestListCompletedManifestCheckpointsBefore verifies filtering and ordering.
func TestListCompletedManifestCheckpointsBefore(t *testing.T) {
	ctx := context.Background()
	h := openTestDB(t)
	q := h.Queries

	repoA := insertTestRepo(t, ctx, q, "/tmp/repoA")
	repoB := insertTestRepo(t, ctx, q, "/tmp/repoB")

	mani := sql.NullString{String: "manifest-hash", Valid: true}

	// Insert repository A checkpoints in sequence order.
	c1, _ := insertCheckpointForQuery(t, ctx, q, repoA, "complete", mani)        // eligible, 0 links
	c2, _ := insertCheckpointForQuery(t, ctx, q, repoA, "complete", mani)        // eligible, 2 links
	insertCheckpointForQuery(t, ctx, q, repoA, "pending", mani)                  // excluded: not complete
	insertCheckpointForQuery(t, ctx, q, repoA, "complete", sql.NullString{})     // excluded: null manifest
	_, targetSeq := insertCheckpointForQuery(t, ctx, q, repoA, "complete", mani) // the boundary (excluded by <)

	// Repository B must not appear in repository A results.
	insertCheckpointForQuery(t, ctx, q, repoB, "complete", mani)

	// Multiple links must not duplicate a checkpoint.
	linkCommit(t, ctx, q, repoA, c2, "commit-aaaa")
	linkCommit(t, ctx, q, repoA, c2, "commit-bbbb")

	rows, err := q.ListCompletedManifestCheckpointsBefore(ctx, sqldb.ListCompletedManifestCheckpointsBeforeParams{
		RepositoryID:       repoA,
		RepositorySequence: targetSeq,
		Limit:              10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// Only c1 and c2 qualify, newest first.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (c2, c1); got %+v", len(rows), rows)
	}
	if rows[0].CheckpointID != c2 || rows[1].CheckpointID != c1 {
		t.Errorf("order = [%s %s], want [c2 c1] newest-first", rows[0].CheckpointID, rows[1].CheckpointID)
	}
	if rows[0].CommitLinkCount != 2 {
		t.Errorf("c2 commit_link_count = %d, want 2", rows[0].CommitLinkCount)
	}
	if rows[1].CommitLinkCount != 0 {
		t.Errorf("c1 commit_link_count = %d, want 0", rows[1].CommitLinkCount)
	}

	// The limit applies after newest-first ordering.
	limited, err := q.ListCompletedManifestCheckpointsBefore(ctx, sqldb.ListCompletedManifestCheckpointsBeforeParams{
		RepositoryID:       repoA,
		RepositorySequence: targetSeq,
		Limit:              1,
	})
	if err != nil {
		t.Fatalf("limited query: %v", err)
	}
	if len(limited) != 1 || limited[0].CheckpointID != c2 {
		t.Errorf("limit=1 = %+v, want only c2", limited)
	}
}
