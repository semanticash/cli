package implementations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TestAttachCommit_AfterSessionCheckpoints exercises the worker ordering used
// in production: observations are reconciled first, then the repo worker
// writes session_checkpoints and commit_links, and only then AttachCommit runs.
// The regression under test is that AttachCommit used to run before
// session_checkpoints existed, which left implementation_commits empty.
func TestAttachCommit_AfterSessionCheckpoints(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Set up a repo with lineage.db.
	repoPath := filepath.Join(dir, "repos", "api")
	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	repoID := uuid.NewString()
	_ = repoH.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: now, EnabledAt: now,
	})

	// Create source, session, checkpoint, commit_link.
	src, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-1", LastSeenAt: now, CreatedAt: now,
	})
	sess, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-1",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	cpID := uuid.NewString()
	_ = repoH.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID, CreatedAt: now,
		Kind: "auto", Status: "pending",
	})
	commitHash := "deadbeef1234567890"
	_ = repoH.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID, CheckpointID: cpID, LinkedAt: now,
	})

	// At this point, session_checkpoints does NOT exist yet.
	// This is the state when reconcileImplementations ran too early.

	_ = sqlstore.Close(repoH)

	// Set up implementations.db with an observation.
	implPath := filepath.Join(dir, "implementations.db")
	implH, _ := impldb.Open(ctx, implPath, impldb.OpenOptions{
		BusyTimeout: 5 * time.Second,
		TxImmediate: true,
	})

	// Insert an observation for this session (as the broker would).
	_ = implH.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     uuid.NewString(),
		Provider:          "claude_code",
		ProviderSessionID: "prov-sess-1",
		SourceProjectPath: impldb.NullStr(repoPath),
		TargetRepoPath:    repoPath,
		EventTs:           now,
		CreatedAt:         now,
	})

	// Reconcile (creates the implementation from the observation).
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string { return "main" },
	}
	result, err := r.Reconcile(ctx, implH)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Processed != 1 {
		t.Fatalf("expected 1 processed observation, got %d", result.Processed)
	}

	// AttachCommit BEFORE session_checkpoints - should find nothing.
	err = r.AttachCommit(ctx, implH, AttachCommitInput{
		RepoPath: repoPath, CommitHash: commitHash,
	})
	if err != nil {
		t.Fatalf("early attach: %v", err)
	}

	// Verify: no commit attached yet (the bug).
	_, findErr := implH.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: broker.CanonicalRepoPath(repoPath),
		CommitHash:    commitHash,
	})
	if findErr == nil {
		t.Fatal("commit should NOT be attached before session_checkpoints exist")
	}

	// Now simulate the worker writing session_checkpoints.
	repoH, _ = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	_ = repoH.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
		SessionID: sess.SessionID, CheckpointID: cpID,
	})
	_ = sqlstore.Close(repoH)

	// AttachCommit AFTER session_checkpoints - should succeed.
	err = r.AttachCommit(ctx, implH, AttachCommitInput{
		RepoPath: repoPath, CommitHash: commitHash,
	})
	if err != nil {
		t.Fatalf("late attach: %v", err)
	}

	_ = impldb.Close(implH)

	// Verify: commit is now attached.
	implH, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(implH) }()

	ownerID, err := implH.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: broker.CanonicalRepoPath(repoPath),
		CommitHash:    commitHash,
	})
	if err != nil {
		t.Fatalf("commit should be attached after session_checkpoints: %v", err)
	}

	// Verify the commit belongs to the implementation that was created.
	impls, _ := implH.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("expected 1 implementation, got %d", len(impls))
	}
	if ownerID != impls[0].ImplementationID {
		t.Errorf("commit attached to %s, expected %s", ownerID[:8], impls[0].ImplementationID[:8])
	}

	// Verify commits count.
	commits, _ := implH.Queries.ListImplementationCommits(ctx, impls[0].ImplementationID)
	if len(commits) != 1 {
		t.Errorf("expected 1 commit in implementation, got %d", len(commits))
	}
	if commits[0].CommitHash != commitHash {
		t.Errorf("commit hash: got %q, want %q", commits[0].CommitHash, commitHash)
	}
}

func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
