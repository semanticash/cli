package implementations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func setupWriteDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	dbPath := filepath.Join(dir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	_ = impldb.Close(h)
	return dir
}

// --- Close ---

func TestClose_SetsClosedState(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	if err := Close(ctx, id); err != nil {
		t.Fatalf("close: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()
	impl, _ := h.Queries.GetImplementation(ctx, id)
	if impl.State != "closed" {
		t.Errorf("expected closed, got %q", impl.State)
	}
	if !impl.ClosedAt.Valid {
		t.Error("expected closed_at to be set")
	}
}

func TestClose_Idempotent(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	if err := Close(ctx, id); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := Close(ctx, id); err != nil {
		t.Fatalf("second close should be idempotent: %v", err)
	}
}

func TestClose_ShortID(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	// Close by short prefix.
	if err := Close(ctx, id[:8]); err != nil {
		t.Fatalf("close by short id: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()
	impl, _ := h.Queries.GetImplementation(ctx, id)
	if impl.State != "closed" {
		t.Errorf("expected closed, got %q", impl.State)
	}
}

// --- Merge ---

func TestMerge_MovesSessionsAndCloseSource(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())

	targetID := uuid.NewString()
	sourceID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: targetID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: sourceID, CreatedAt: now, LastActivityAt: now + 5000,
	})

	// Source has a session, repo, branch, and commit.
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: sourceID, Provider: "claude_code",
		ProviderSessionID: "sess-src", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: sourceID, CanonicalPath: "/repos/sdk", DisplayName: "sdk",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
		ImplementationID: sourceID, CanonicalPath: "/repos/sdk",
		Branch: "feature/x", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: sourceID, CanonicalPath: "/repos/sdk",
		CommitHash: "abc123", AttachedAt: now, AttachRule: "session_identity",
	})

	// Target has a different session and repo.
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: targetID, Provider: "cursor",
		ProviderSessionID: "sess-tgt", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: targetID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})

	_ = impldb.Close(h)

	// Merge source into target.
	result, err := Merge(ctx, targetID, sourceID)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if result.TargetID != targetID || result.SourceID != sourceID {
		t.Errorf("unexpected result IDs")
	}

	// Verify.
	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	// Source is closed.
	source, _ := h.Queries.GetImplementation(ctx, sourceID)
	if source.State != "closed" {
		t.Errorf("source state: got %q, want closed", source.State)
	}

	// Target has both sessions.
	sessions, _ := h.Queries.ListProviderSessionsForImplementation(ctx, targetID)
	if len(sessions) != 2 {
		t.Errorf("target sessions: got %d, want 2", len(sessions))
	}

	// Target has both repos.
	repos, _ := h.Queries.ListImplementationRepos(ctx, targetID)
	if len(repos) != 2 {
		t.Errorf("target repos: got %d, want 2", len(repos))
	}

	// Target has the commit.
	commits, _ := h.Queries.ListImplementationCommits(ctx, targetID)
	if len(commits) != 1 || commits[0].CommitHash != "abc123" {
		t.Errorf("target commits: got %v", commits)
	}

	// Target has the branch.
	branches, _ := h.Queries.ListBranchesForImplementation(ctx, targetID)
	if len(branches) != 1 || branches[0].Branch != "feature/x" {
		t.Errorf("target branches: got %v", branches)
	}

	// Target last_activity_at updated to source's (which was later).
	target, _ := h.Queries.GetImplementation(ctx, targetID)
	if target.LastActivityAt != now+5000 {
		t.Errorf("target last_activity_at: got %d, want %d", target.LastActivityAt, now+5000)
	}
}

func TestMerge_PreservesOriginRole(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())

	targetID := uuid.NewString()
	sourceID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: targetID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: sourceID, CreatedAt: now, LastActivityAt: now,
	})

	// Target has repo as downstream.
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: targetID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now,
	})
	// Source has same repo as origin.
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: sourceID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now - 1000, LastSeenAt: now,
	})

	_ = impldb.Close(h)

	_, err := Merge(ctx, targetID, sourceID)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	repos, _ := h.Queries.ListImplementationRepos(ctx, targetID)
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0].RepoRole != "origin" {
		t.Errorf("expected origin (promoted from source), got %q", repos[0].RepoRole)
	}
}

func TestMerge_SelfMergeErrors(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := Merge(ctx, id, id)
	if err == nil {
		t.Error("expected error for self-merge")
	}
}

func TestMerge_DuplicateSessionHandled(t *testing.T) {
	dir := setupWriteDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())

	targetID := uuid.NewString()
	sourceID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: targetID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: sourceID, CreatedAt: now, LastActivityAt: now,
	})

	// Same session in both implementations (can happen after close+reattach).
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: targetID, Provider: "claude_code",
		ProviderSessionID: "sess-dup", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: sourceID, Provider: "claude_code",
		ProviderSessionID: "sess-dup", AttachRule: "branch_active", AttachedAt: now,
	})

	_ = impldb.Close(h)

	// Merge should not fail on duplicate session.
	_, err := Merge(ctx, targetID, sourceID)
	if err != nil {
		t.Fatalf("merge with duplicate session: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	sessions, _ := h.Queries.ListProviderSessionsForImplementation(ctx, targetID)
	if len(sessions) != 1 {
		t.Errorf("expected 1 session (deduped), got %d", len(sessions))
	}
}

func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
