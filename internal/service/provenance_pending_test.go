package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestPendingProvenance_CountsAllAndSinceLastCommit(t *testing.T) {
	ctx := context.Background()
	repoRoot, repoID, h := setupPendingProvenanceRepo(t, ctx)
	defer func() { _ = sqlstore.Close(h) }()

	insertManifestForPendingTest(t, ctx, h, repoID, "old-packaged", "packaged", 1_000)
	insertCommitCheckpointForPendingTest(t, ctx, h, repoID, "commit-cp", "abc123", 2_000)
	insertManifestForPendingTest(t, ctx, h, repoID, "new-packaged", "packaged", 3_000)
	insertManifestForPendingTest(t, ctx, h, repoID, "new-uploading", "uploading", 4_000)
	insertManifestForPendingTest(t, ctx, h, repoID, "new-uploaded", "uploaded", 5_000)

	info, err := PendingProvenance(ctx, repoRoot)
	if err != nil {
		t.Fatalf("PendingProvenance: %v", err)
	}
	if info.Count != 3 {
		t.Fatalf("Count = %d, want 3", info.Count)
	}
	if !info.HasLastCommit {
		t.Fatal("HasLastCommit = false, want true")
	}
	if info.SinceLastCommitCount != 2 {
		t.Fatalf("SinceLastCommitCount = %d, want 2", info.SinceLastCommitCount)
	}
	if info.LastCommitAt != 2_000 {
		t.Fatalf("LastCommitAt = %d, want 2000", info.LastCommitAt)
	}
}

func setupPendingProvenanceRepo(t *testing.T, ctx context.Context) (string, string, *sqlstore.Handle) {
	t.Helper()

	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatalf("mkdir semantica dir: %v", err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoRoot,
		CreatedAt:    1,
		EnabledAt:    1,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return repoRoot, repoID, h
}

func insertCommitCheckpointForPendingTest(
	t *testing.T,
	ctx context.Context,
	h *sqlstore.Handle,
	repoID, checkpointID, commitHash string,
	createdAt int64,
) {
	t.Helper()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: checkpointID,
		RepositoryID: repoID,
		CreatedAt:    createdAt,
		Kind:         "auto",
		Status:       "complete",
		CompletedAt:  sql.NullInt64{Int64: createdAt + 1, Valid: true},
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: repoID,
		CheckpointID: checkpointID,
		LinkedAt:     createdAt + 2,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
}

func insertManifestForPendingTest(
	t *testing.T,
	ctx context.Context,
	h *sqlstore.Handle,
	repoID, turnID, status string,
	createdAt int64,
) {
	t.Helper()

	sessionID := "session-" + turnID
	source, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoID,
		Provider:     "claude-code",
		SourceKey:    "test-source",
		LastSeenAt:   createdAt,
		CreatedAt:    createdAt,
	})
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         sessionID,
		ProviderSessionID: sessionID,
		RepositoryID:      repoID,
		Provider:          "claude-code",
		SourceID:          source.SourceID,
		StartedAt:         createdAt,
		LastSeenAt:        createdAt,
		MetadataJson:      "{}",
	}); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
		ManifestID:   uuid.NewString(),
		RepositoryID: repoID,
		SessionID:    sessionID,
		TurnID:       turnID,
		Provider:     "claude-code",
		Kind:         "turn_bundle",
		StartedAt:    createdAt,
		Status:       status,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}); err != nil {
		t.Fatalf("insert manifest %s: %v", turnID, err)
	}
}
