package commands

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/spf13/cobra"
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

func TestPendingProvenanceSentence(t *testing.T) {
	tests := []struct {
		name string
		info *service.PendingProvenanceInfo
		want string
	}{
		{
			name: "all since last commit",
			info: &service.PendingProvenanceInfo{
				Count:                2,
				HasLastCommit:        true,
				SinceLastCommitCount: 2,
			},
			want: "Since your last commit, 2 provenance turns pending locally.",
		},
		{
			name: "mixed older and since last commit",
			info: &service.PendingProvenanceInfo{
				Count:                5,
				HasLastCommit:        true,
				SinceLastCommitCount: 3,
			},
			want: "5 provenance turns pending locally, including 3 since your last commit.",
		},
		{
			name: "no last commit",
			info: &service.PendingProvenanceInfo{Count: 1},
			want: "1 provenance turn pending locally.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pendingProvenanceSentence(tc.info); got != tc.want {
				t.Fatalf("pendingProvenanceSentence = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandleAlreadyConnectedProvenance_NonInteractiveDoesNotSync(t *testing.T) {
	ctx := context.Background()
	repoRoot, repoID, h := setupConnectPendingRepo(t, ctx)
	defer func() { _ = sqlstore.Close(h) }()

	insertConnectPendingManifest(t, ctx, h, repoID, "turn-1", "packaged", time.Now().UnixMilli())

	called := false
	origSync := connectProvenanceSyncFn
	connectProvenanceSyncFn = func(_ *cobra.Command, _, _ string) {
		called = true
	}
	t.Cleanup(func() { connectProvenanceSyncFn = origSync })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("y\n"))

	if err := handleAlreadyConnectedProvenance(cmd, filepath.Join(repoRoot, ".semantica")); err != nil {
		t.Fatalf("handleAlreadyConnectedProvenance: %v", err)
	}
	if called {
		t.Fatal("sync should not run for non-interactive callers")
	}
	got := out.String()
	for _, want := range []string{
		"Already connected.",
		"1 provenance turn pending locally.",
		"interactive terminal",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestHandleAlreadyConnectedProvenance_InteractiveAcceptsSync(t *testing.T) {
	ctx := context.Background()
	repoRoot, repoID, h := setupConnectPendingRepo(t, ctx)
	defer func() { _ = sqlstore.Close(h) }()

	insertConnectCommitCheckpoint(t, ctx, h, repoID, "cp-1", "abc123", 1_000)
	insertConnectPendingManifest(t, ctx, h, repoID, "turn-1", "packaged", 2_000)

	origInteractive := isInteractiveCmdFn
	isInteractiveCmdFn = func(_ *cobra.Command) bool { return true }
	t.Cleanup(func() { isInteractiveCmdFn = origInteractive })

	origConfirm := confirmPendingProvenanceSyncFn
	confirmPendingProvenanceSyncFn = func(_ *cobra.Command, info *service.PendingProvenanceInfo) (bool, error) {
		if info.Count != 1 || info.SinceLastCommitCount != 1 {
			t.Fatalf("pending info = %+v, want one turn since last commit", info)
		}
		return true, nil
	}
	t.Cleanup(func() { confirmPendingProvenanceSyncFn = origConfirm })

	var syncedRepoRoot, failureLabel string
	origSync := connectProvenanceSyncFn
	connectProvenanceSyncFn = func(_ *cobra.Command, repoRoot, label string) {
		syncedRepoRoot = repoRoot
		failureLabel = label
	}
	t.Cleanup(func() { connectProvenanceSyncFn = origSync })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := handleAlreadyConnectedProvenance(cmd, filepath.Join(repoRoot, ".semantica")); err != nil {
		t.Fatalf("handleAlreadyConnectedProvenance: %v", err)
	}
	if syncedRepoRoot != repoRoot {
		t.Fatalf("synced repo root = %q, want %q", syncedRepoRoot, repoRoot)
	}
	if failureLabel != "provenance sync failed" {
		t.Fatalf("failure label = %q, want provenance sync failed", failureLabel)
	}
	if !strings.Contains(out.String(), "Since your last commit, 1 provenance turn pending locally.") {
		t.Fatalf("output missing pending sentence:\n%s", out.String())
	}
}

func TestHandleAlreadyConnectedProvenance_InteractiveDeclinesSync(t *testing.T) {
	ctx := context.Background()
	repoRoot, repoID, h := setupConnectPendingRepo(t, ctx)
	defer func() { _ = sqlstore.Close(h) }()

	insertConnectPendingManifest(t, ctx, h, repoID, "turn-1", "packaged", time.Now().UnixMilli())

	origInteractive := isInteractiveCmdFn
	isInteractiveCmdFn = func(_ *cobra.Command) bool { return true }
	t.Cleanup(func() { isInteractiveCmdFn = origInteractive })

	origConfirm := confirmPendingProvenanceSyncFn
	confirmPendingProvenanceSyncFn = func(_ *cobra.Command, _ *service.PendingProvenanceInfo) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { confirmPendingProvenanceSyncFn = origConfirm })

	called := false
	origSync := connectProvenanceSyncFn
	connectProvenanceSyncFn = func(_ *cobra.Command, _, _ string) {
		called = true
	}
	t.Cleanup(func() { connectProvenanceSyncFn = origSync })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := handleAlreadyConnectedProvenance(cmd, filepath.Join(repoRoot, ".semantica")); err != nil {
		t.Fatalf("handleAlreadyConnectedProvenance: %v", err)
	}
	if called {
		t.Fatal("sync should not run when the user declines")
	}
	if !strings.Contains(out.String(), "Skipped provenance sync.") {
		t.Fatalf("output missing decline message:\n%s", out.String())
	}
}

func TestHandleAlreadyConnectedProvenance_InspectErrorReturnsNil(t *testing.T) {
	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatalf("mkdir semantica dir: %v", err)
	}

	called := false
	origSync := connectProvenanceSyncFn
	connectProvenanceSyncFn = func(_ *cobra.Command, _, _ string) {
		called = true
	}
	t.Cleanup(func() { connectProvenanceSyncFn = origSync })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := handleAlreadyConnectedProvenance(cmd, semDir); err != nil {
		t.Fatalf("handleAlreadyConnectedProvenance: %v", err)
	}
	if called {
		t.Fatal("sync should not run when pending inspection fails")
	}
	got := out.String()
	for _, want := range []string{
		"Already connected.",
		"Note: could not inspect pending provenance:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func setupConnectPendingRepo(t *testing.T, ctx context.Context) (string, string, *sqlstore.Handle) {
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
	now := time.Now().UnixMilli()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoRoot,
		CreatedAt:    now,
		EnabledAt:    now,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return repoRoot, repoID, h
}

func insertConnectCommitCheckpoint(
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

func insertConnectPendingManifest(
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
