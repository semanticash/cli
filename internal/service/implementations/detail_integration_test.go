package implementations

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TestGetDetail_PinsFullBehavior is an integration test that pins the return
// shape of GetDetail including timeline, tokens, attribution, and commits.
// Run before any refactoring to verify no behavior change.
func TestGetDetail_PinsFullBehavior(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()

	// Set up a git repo so lookupCommitSubject works.
	repoPath := filepath.Join(dir, "repos", "myrepo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitRun("add", "main.go")
	gitRun("commit", "-m", "add main.go")
	commitOut := gitRun("rev-parse", "HEAD")
	commitHash := string([]byte(commitOut[:40])) // trim newline

	now := time.Now().UnixMilli()

	// Create implementations.db with one implementation.
	implPath := filepath.Join(dir, "implementations.db")
	implH, err := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	implID := uuid.NewString()
	_ = implH.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID,
		CreatedAt:        now - 10000,
		LastActivityAt:   now - 1000,
	})

	// Add repo.
	canonicalPath := repoPath
	_ = implH.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID,
		CanonicalPath:    canonicalPath,
		DisplayName:      "myrepo",
		RepoRole:         "origin",
		FirstSeenAt:      now - 10000,
		LastSeenAt:       now - 1000,
	})

	// Add session.
	sessID := uuid.NewString()
	providerSessionID := "prov-sess-detail-test"
	_ = implH.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  implID,
		Provider:          "claude_code",
		ProviderSessionID: providerSessionID,
		AttachRule:        "direct",
		AttachedAt:        now - 9000,
	})

	// Add repo session link.
	_ = implH.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID:  implID,
		Provider:          "claude_code",
		ProviderSessionID: providerSessionID,
		CanonicalPath:     canonicalPath,
		SessionID:         sessID,
		FirstSeenAt:       now - 9000,
		LastSeenAt:        now - 1000,
	})

	// Add commit.
	_ = implH.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: implID,
		CanonicalPath:    canonicalPath,
		CommitHash:       commitHash,
		AttachedAt:       now - 1000,
		AttachRule:       "session_overlap",
	})

	_ = impldb.Close(implH)

	// Create lineage.db inside the repo's .semantica dir.
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

	src, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-1",
		LastSeenAt: now, CreatedAt: now,
	})

	repoSess, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: sessID, ProviderSessionID: providerSessionID,
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now - 9000, LastSeenAt: now,
	})
	_ = repoSess

	// Insert an event so timeline has content.
	_ = repoH.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      uuid.NewString(),
		SessionID:    sessID,
		RepositoryID: repoID,
		Ts:           now - 5000,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolName:     sql.NullString{String: "Write", Valid: true},
		Summary:      sql.NullString{String: "wrote main.go", Valid: true},
		EventSource:  "hook",
	})

	// Insert a checkpoint + commit link + stats for attribution.
	cpID := uuid.NewString()
	_ = repoH.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID,
		CreatedAt: now - 2000, Kind: "auto", Status: "complete",
	})
	_ = repoH.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID,
		CheckpointID: cpID, LinkedAt: now - 1000,
	})
	_ = repoH.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cpID, SessionCount: 1, FilesChanged: 1,
	})
	_ = repoH.Queries.UpdateCheckpointAIPercentage(ctx, sqldb.UpdateCheckpointAIPercentageParams{
		AiPercentage: 75.0, CheckpointID: cpID,
	})

	_ = sqlstore.Close(repoH)

	// Call GetDetail.
	detail, err := GetDetail(ctx, implID)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}

	// Pin: implementation metadata.
	if detail.ImplementationID != implID {
		t.Errorf("ImplementationID = %q, want %q", detail.ImplementationID, implID)
	}
	if detail.State != "active" {
		t.Errorf("State = %q, want active", detail.State)
	}

	// Pin: repos.
	if len(detail.Repos) != 1 {
		t.Fatalf("Repos = %d, want 1", len(detail.Repos))
	}
	if detail.Repos[0].DisplayName != "myrepo" {
		t.Errorf("Repo.DisplayName = %q, want myrepo", detail.Repos[0].DisplayName)
	}
	if detail.Repos[0].Role != "origin" {
		t.Errorf("Repo.Role = %q, want origin", detail.Repos[0].Role)
	}

	// Pin: sessions.
	if len(detail.Sessions) != 1 {
		t.Fatalf("Sessions = %d, want 1", len(detail.Sessions))
	}
	if detail.Sessions[0].Provider != "claude_code" {
		t.Errorf("Session.Provider = %q, want claude_code", detail.Sessions[0].Provider)
	}

	// Pin: commits.
	if len(detail.Commits) != 1 {
		t.Fatalf("Commits = %d, want 1", len(detail.Commits))
	}
	if detail.Commits[0].CommitHash != commitHash {
		t.Errorf("Commit.Hash = %q, want %q", detail.Commits[0].CommitHash, commitHash)
	}
	if detail.Commits[0].Subject != "add main.go" {
		t.Errorf("Commit.Subject = %q, want 'add main.go'", detail.Commits[0].Subject)
	}
	if detail.Commits[0].DisplayName != "myrepo" {
		t.Errorf("Commit.DisplayName = %q, want myrepo", detail.Commits[0].DisplayName)
	}

	// Pin: timeline has at least the event + commit.
	if len(detail.Timeline) < 2 {
		t.Fatalf("Timeline = %d, want >= 2 (event + commit)", len(detail.Timeline))
	}
	hasToolEntry := false
	hasCommitEntry := false
	for _, e := range detail.Timeline {
		if e.Kind == "tool" {
			hasToolEntry = true
		}
		if e.Kind == "commit" {
			hasCommitEntry = true
		}
	}
	if !hasToolEntry {
		t.Error("Timeline missing tool entry")
	}
	if !hasCommitEntry {
		t.Error("Timeline missing commit entry")
	}

	// Pin: attribution.
	if len(detail.RepoAttribution) != 1 {
		t.Fatalf("RepoAttribution = %d, want 1", len(detail.RepoAttribution))
	}
	if detail.RepoAttribution[0].AIPercentage != 75.0 {
		t.Errorf("AIPercentage = %.1f, want 75.0", detail.RepoAttribution[0].AIPercentage)
	}
	if detail.RepoAttribution[0].CommitCount != 1 {
		t.Errorf("CommitCount = %d, want 1", detail.RepoAttribution[0].CommitCount)
	}
}
