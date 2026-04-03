package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestInitBackfillState_NoCommitLinks(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)

	hasBacklog, err := InitBackfillState(ctx, h, "hosted-repo-1", repoID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasBacklog {
		t.Fatal("expected no backlog when no commit links exist")
	}
}

func TestInitBackfillState_WithCommitLinks(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)
	cp1 := insertCheckpoint(t, h, repoID, 200_000, "auto")
	cp2 := insertCheckpoint(t, h, repoID, 300_000, "auto")

	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "aaa111", RepositoryID: repoID, CheckpointID: cp1, LinkedAt: 200_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "bbb222", RepositoryID: repoID, CheckpointID: cp2, LinkedAt: 300_000,
	}); err != nil {
		t.Fatal(err)
	}

	hasBacklog, err := InitBackfillState(ctx, h, "hosted-repo-1", repoID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasBacklog {
		t.Fatal("expected backlog to exist")
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-repo-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.CutoffCommitHash != "bbb222" {
		t.Errorf("expected cutoff commit bbb222, got %s", bf.CutoffCommitHash)
	}
	if bf.CutoffLinkedAt != 300_000 {
		t.Errorf("expected cutoff linked_at 300000, got %d", bf.CutoffLinkedAt)
	}
	if bf.Status != "pending" {
		t.Errorf("expected pending, got %s", bf.Status)
	}
	if bf.CursorLinkedAt != 0 || bf.CursorCommitHash != "" {
		t.Errorf("expected zero cursor, got (%d, %s)", bf.CursorLinkedAt, bf.CursorCommitHash)
	}
}

func TestUpsertBackfill_ExtendsCutoff(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)
	cp1 := insertCheckpoint(t, h, repoID, 200_000, "auto")

	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "aaa111", RepositoryID: repoID, CheckpointID: cp1, LinkedAt: 200_000,
	}); err != nil {
		t.Fatal(err)
	}

	hasBacklog, err := InitBackfillState(ctx, h, "hosted-1", repoID)
	if err != nil || !hasBacklog {
		t.Fatalf("init: hasBacklog=%v err=%v", hasBacklog, err)
	}

	// Extend with a newer cutoff.
	if err := ExtendBackfillCutoff(ctx, h, "hosted-1", repoID, "ccc333", 400_000); err != nil {
		t.Fatalf("extend: %v", err)
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.CutoffCommitHash != "ccc333" || bf.CutoffLinkedAt != 400_000 {
		t.Errorf("cutoff not extended: got (%s, %d)", bf.CutoffCommitHash, bf.CutoffLinkedAt)
	}
}

func TestUpsertBackfill_NeverShrinksCutoff(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)

	now := time.Now().UnixMilli()
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc333", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Try to shrink with an older cutoff.
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 200_000, CutoffCommitHash: "aaa111", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.CutoffLinkedAt != 400_000 || bf.CutoffCommitHash != "ccc333" {
		t.Errorf("cutoff should not shrink: got (%d, %s)", bf.CutoffLinkedAt, bf.CutoffCommitHash)
	}
}

func TestUpsertBackfill_ReopensCompleteOnExtend(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)

	now := time.Now().UnixMilli()
	// Create initial state and mark complete.
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 200_000, CutoffCommitHash: "aaa111", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.CompleteBackfill(ctx, sqldb.CompleteBackfillParams{
		UpdatedAt: now, ConnectedRepoID: "hosted-1",
	}); err != nil {
		t.Fatal(err)
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.Status != "complete" {
		t.Fatalf("expected complete, got %s", bf.Status)
	}

	// Extend cutoff beyond current - should reopen to pending.
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc333", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	bf, err = h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.Status != "pending" {
		t.Errorf("expected pending after cutoff extension, got %s", bf.Status)
	}
	if bf.CutoffLinkedAt != 400_000 {
		t.Errorf("expected extended cutoff 400000, got %d", bf.CutoffLinkedAt)
	}
}

func TestUpsertBackfill_StaysCompleteOnSameCutoff(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)

	now := time.Now().UnixMilli()
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 200_000, CutoffCommitHash: "aaa111", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.CompleteBackfill(ctx, sqldb.CompleteBackfillParams{
		UpdatedAt: now, ConnectedRepoID: "hosted-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with same cutoff - should stay complete.
	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 200_000, CutoffCommitHash: "aaa111", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.Status != "complete" {
		t.Errorf("expected complete when cutoff unchanged, got %s", bf.Status)
	}
}

func TestListBackfillReplayCandidates(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)

	// Create 3 commit links.
	for i, hash := range []string{"aaa", "bbb", "ccc"} {
		cp := insertCheckpoint(t, h, repoID, int64(200_000+i*100_000), "auto")
		if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
			CommitHash: hash, RepositoryID: repoID, CheckpointID: cp, LinkedAt: int64(200_000 + i*100_000),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Full window: cursor at 0, cutoff at ccc.
	rows, err := h.Queries.ListBackfillReplayCandidates(ctx, sqldb.ListBackfillReplayCandidatesParams{
		RepositoryID: repoID, CursorLinkedAt: 0, CursorCommitHash: "",
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc", BatchLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(rows))
	}
	if rows[0].CommitHash != "aaa" || rows[2].CommitHash != "ccc" {
		t.Errorf("unexpected order: %v", rows)
	}

	// Advance cursor past aaa.
	rows, err = h.Queries.ListBackfillReplayCandidates(ctx, sqldb.ListBackfillReplayCandidatesParams{
		RepositoryID: repoID, CursorLinkedAt: 200_000, CursorCommitHash: "aaa",
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc", BatchLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 after cursor advance, got %d", len(rows))
	}
	if rows[0].CommitHash != "bbb" {
		t.Errorf("expected bbb first, got %s", rows[0].CommitHash)
	}
}

func TestAdvanceBackfillCursor_ClearsFailureState(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)
	now := time.Now().UnixMilli()

	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc333", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Record a failure.
	if err := h.Queries.RecordBackfillFailure(ctx, sqldb.RecordBackfillFailureParams{
		FailedCommitHash: sql.NullString{String: "aaa111", Valid: true},
		LastError:        sql.NullString{String: "timeout", Valid: true},
		UpdatedAt:        now, ConnectedRepoID: "hosted-1",
	}); err != nil {
		t.Fatal(err)
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.RetryAttempts != 1 || !bf.FailedCommitHash.Valid {
		t.Fatalf("expected failure recorded: attempts=%d failed=%v", bf.RetryAttempts, bf.FailedCommitHash)
	}

	// Advance cursor - should clear failure state.
	if err := h.Queries.AdvanceBackfillCursor(ctx, sqldb.AdvanceBackfillCursorParams{
		CursorLinkedAt: 200_000, CursorCommitHash: "aaa111",
		UpdatedAt: now, ConnectedRepoID: "hosted-1",
	}); err != nil {
		t.Fatal(err)
	}

	bf, err = h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.RetryAttempts != 0 {
		t.Errorf("expected retry_attempts cleared, got %d", bf.RetryAttempts)
	}
	if bf.FailedCommitHash.Valid {
		t.Errorf("expected failed_commit_hash cleared, got %s", bf.FailedCommitHash.String)
	}
	if bf.LastError.Valid {
		t.Errorf("expected last_error cleared, got %s", bf.LastError.String)
	}
	if bf.CursorLinkedAt != 200_000 || bf.CursorCommitHash != "aaa111" {
		t.Errorf("cursor not advanced: (%d, %s)", bf.CursorLinkedAt, bf.CursorCommitHash)
	}
}

func TestRecordBackfillFailure_IncrementsRetry(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	repoID := insertRepo(t, h, 100_000)
	now := time.Now().UnixMilli()

	if err := h.Queries.UpsertAttributionBackfill(ctx, sqldb.UpsertAttributionBackfillParams{
		ConnectedRepoID: "hosted-1", RepositoryID: repoID,
		CutoffLinkedAt: 400_000, CutoffCommitHash: "ccc333", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := h.Queries.RecordBackfillFailure(ctx, sqldb.RecordBackfillFailureParams{
			FailedCommitHash: sql.NullString{String: "aaa111", Valid: true},
			LastError:        sql.NullString{String: "timeout", Valid: true},
			UpdatedAt:        now, ConnectedRepoID: "hosted-1",
		}); err != nil {
			t.Fatal(err)
		}
	}

	bf, err := h.Queries.GetAttributionBackfill(ctx, "hosted-1")
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	if bf.RetryAttempts != 3 {
		t.Errorf("expected 3 retry attempts, got %d", bf.RetryAttempts)
	}
	if !bf.FailedCommitHash.Valid || bf.FailedCommitHash.String != "aaa111" {
		t.Errorf("expected failed_commit_hash aaa111, got %v", bf.FailedCommitHash)
	}
}
