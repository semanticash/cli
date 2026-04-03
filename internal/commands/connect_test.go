package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TestBackfillAttribution_UsesLocalRepoID verifies that the connect-side
// backfill seeding looks up the local SQLite repository_id (from
// GetRepositoryByRootPath) rather than using the hosted connected_repo_id.
// The hosted ID lives in a different namespace than commit_links.repository_id.
func TestBackfillAttribution_UsesLocalRepoID(t *testing.T) {
	ctx := context.Background()

	// Set up a temp directory mimicking a repo root with .semantica/lineage.db.
	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Insert a local repository whose RootPath matches repoRoot.
	localRepoID := uuid.NewString()
	now := time.Now().UnixMilli()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: localRepoID,
		RootPath:     repoRoot,
		CreatedAt:    now,
		EnabledAt:    now,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Insert a checkpoint and commit link using the local repo ID.
	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: localRepoID,
		CreatedAt:    now,
		Kind:         "auto",
		Status:       "complete",
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "abc123",
		RepositoryID: localRepoID,
		CheckpointID: cpID,
		LinkedAt:     now,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	_ = sqlstore.Close(h)

	// Call backfillAttribution with a hosted connected_repo_id that differs
	// from the local SQLite repository_id. The function must use the local
	// one for commit_links queries.
	hostedConnectedRepoID := "hosted-" + uuid.NewString()
	var buf bytes.Buffer
	backfillAttribution(ctx, &buf, semDir, hostedConnectedRepoID)

	// Re-open the DB and verify the backfill row was created with the
	// correct local repository_id, not the hosted one.
	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	bf, err := h.Queries.GetAttributionBackfill(ctx, hostedConnectedRepoID)
	if err != nil {
		t.Fatalf("backfill row should exist: %v", err)
	}

	if bf.RepositoryID != localRepoID {
		t.Errorf("backfill.repository_id = %q, want local repo ID %q (not the hosted connected_repo_id)", bf.RepositoryID, localRepoID)
	}
	if bf.CutoffCommitHash != "abc123" {
		t.Errorf("backfill.cutoff_commit_hash = %q, want abc123", bf.CutoffCommitHash)
	}
	if bf.Status != "pending" {
		t.Errorf("backfill.status = %q, want pending", bf.Status)
	}
}

// TestBackfillAttribution_NoRepo verifies that backfillAttribution is a
// no-op when the local DB has no repository row for the repo root (e.g.,
// connect before enable finishes writing the DB).
func TestBackfillAttribution_NoRepo(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Call with no repo row - should not panic or create a backfill row.
	var buf bytes.Buffer
	backfillAttribution(ctx, &buf, semDir, "hosted-id")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	_, err = h.Queries.GetAttributionBackfill(ctx, "hosted-id")
	if err == nil {
		t.Error("expected no backfill row when repo is missing from DB")
	}
}
