package service

import (
	"context"
	"os"
	"os/exec"
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

// TestWorkerRun_AttachesCommitToImplementation verifies the worker-level flow:
// once WorkerService.Run completes, implementation_commits contains the
// commit row and the required session_checkpoints dependency has been written.
func TestWorkerRun_AttachesCommitToImplementation(t *testing.T) {
	// Set up an isolated git repo.
	dir := t.TempDir()
	globalDir := filepath.Join(dir, ".semantica-global")
	t.Setenv("SEMANTICA_HOME", globalDir)
	t.Setenv("HOME", dir)

	repoDir := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", repoDir},
		{"-C", repoDir, "config", "user.email", "test@test.com"},
		{"-C", repoDir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create a file and commit.
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644)
	for _, args := range [][]string{
		{"-C", repoDir, "add", "main.go"},
		{"-C", repoDir, "commit", "-m", "add main.go"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Get the commit hash.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	commitHash := string(out)
	if len(commitHash) > 0 && commitHash[len(commitHash)-1] == '\n' {
		commitHash = commitHash[:len(commitHash)-1]
	}

	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Enable the local Semantica state needed by the worker.
	semDir := filepath.Join(repoDir, ".semantica")
	_ = os.MkdirAll(semDir, 0o755)
	_ = os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644)
	_ = os.MkdirAll(filepath.Join(semDir, "objects"), 0o755)

	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage db: %v", err)
	}

	repoID := uuid.NewString()
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoDir, CreatedAt: now, EnabledAt: now,
	})

	// Create source + session + event so the worker finds sessions in window.
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-1", LastSeenAt: now, CreatedAt: now,
	})
	sess, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-worker",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now - 1000, LastSeenAt: now,
	})

	// Insert an event so the session appears in the window.
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      uuid.NewString(),
		SessionID:    sess.SessionID,
		RepositoryID: repoID,
		Ts:           now - 500,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		EventSource:  "hook",
	})

	// Create a pending checkpoint and commit link (simulating post-commit hook).
	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID, CreatedAt: now,
		Kind: "auto", Status: "pending",
	})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID, CheckpointID: cpID, LinkedAt: now,
	})
	_ = sqlstore.Close(h)

	// Register the repo in the broker.
	regPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, regPath)
	_ = broker.Register(ctx, bh, repoDir, repoDir)
	_ = broker.Close(bh)

	// Create implementations.db with an observation for this session.
	implPath := filepath.Join(globalDir, "implementations.db")
	implH, err := impldb.Open(ctx, implPath, impldb.OpenOptions{
		BusyTimeout: 5 * time.Second,
		TxImmediate: true,
	})
	if err != nil {
		t.Fatalf("create impldb: %v", err)
	}
	_ = implH.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     uuid.NewString(),
		Provider:          "claude_code",
		ProviderSessionID: "prov-sess-worker",
		SourceProjectPath: impldb.NullStr(repoDir),
		TargetRepoPath:    repoDir,
		EventTs:           now,
		CreatedAt:         now,
	})
	_ = impldb.Close(implH)

	// Run the worker.
	svc := NewWorkerService()
	if err := svc.Run(ctx, WorkerInput{
		CheckpointID: cpID,
		CommitHash:   commitHash,
		RepoRoot:     repoDir,
	}); err != nil {
		t.Fatalf("worker.Run: %v", err)
	}

	// Verify that implementation_commits has the row.
	implH, err = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen impldb: %v", err)
	}
	defer func() { _ = impldb.Close(implH) }()

	impls, _ := implH.Queries.ListAllImplementations(ctx, 10)
	if len(impls) == 0 {
		t.Fatal("expected at least 1 implementation after worker run")
	}

	canonicalPath := broker.CanonicalRepoPath(repoDir)
	ownerID, err := implH.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: canonicalPath,
		CommitHash:    commitHash,
	})
	if err != nil {
		t.Fatalf("commit not attached to any implementation: %v", err)
	}

	commits, _ := implH.Queries.ListImplementationCommits(ctx, ownerID)
	if len(commits) != 1 {
		t.Errorf("expected 1 commit in implementation, got %d", len(commits))
	}
	if commits[0].CommitHash != commitHash {
		t.Errorf("commit hash: got %q, want %q", commits[0].CommitHash, commitHash)
	}

	// Also verify session_checkpoints were written.
	h, _ = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	defer func() { _ = sqlstore.Close(h) }()

	sessForCP, _ := h.Queries.ListSessionsForCheckpoint(ctx, cpID)
	if len(sessForCP) == 0 {
		t.Error("expected session_checkpoints to exist after worker run")
	}
}

// Suppress settings.json auto-detection of providers
func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
