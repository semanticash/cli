package implementations

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// setupLinkEnv creates a SEMANTICA_HOME with implementations.db, a repo with
// lineage.db containing sessions, and returns (globalDir, repoPath, session info).
type testSession struct {
	localID           string // Semantica session UUID in lineage.db
	provider          string
	providerSessionID string
}

func setupLinkEnv(t *testing.T) (string, string, []testSession) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()

	// Create implementations.db.
	implPath := filepath.Join(dir, "implementations.db")
	implH, err := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	_ = impldb.Close(implH)

	// Create a repo with lineage.db and sessions.
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

	now := time.Now().UnixMilli()
	repoID := uuid.NewString()
	_ = repoH.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: now, EnabledAt: now,
	})

	src, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-1", LastSeenAt: now, CreatedAt: now,
	})

	sessA, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-A",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now, LastSeenAt: now,
	})

	sessB, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-B",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now, LastSeenAt: now,
	})

	_ = sqlstore.Close(repoH)

	sessions := []testSession{
		{localID: sessA.SessionID, provider: "claude_code", providerSessionID: "prov-sess-A"},
		{localID: sessB.SessionID, provider: "claude_code", providerSessionID: "prov-sess-B"},
	}

	return dir, repoPath, sessions
}

// --- LinkSession: fresh link by Semantica UUID ---

func TestLinkSession_ByLocalUUID(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Register repo in broker so findAllRepoSlices can discover it.
	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	result, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if result.LinkedProvider != "claude_code" {
		t.Errorf("provider: got %q", result.LinkedProvider)
	}
	if result.MovedFrom != "" {
		t.Errorf("expected fresh link, got movedFrom=%q", result.MovedFrom)
	}

	// Verify provider session and repo session were created.
	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	provSess, _ := h.Queries.ListProviderSessionsForImplementation(ctx, implID)
	if len(provSess) != 1 {
		t.Fatalf("expected 1 provider session, got %d", len(provSess))
	}

	repoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(repoSess) != 1 {
		t.Fatalf("expected 1 repo session, got %d", len(repoSess))
	}
	if repoSess[0].SessionID != sessions[0].localID {
		t.Errorf("repo session ID: got %q, want %q", repoSess[0].SessionID, sessions[0].localID)
	}

	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
}

// --- LinkSession: by provider_session_id ---

func TestLinkSession_ByProviderSessionID(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	// Link by provider_session_id instead of local UUID.
	result, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].providerSessionID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("link by provider session id: %v", err)
	}
	if result.LinkedSessionID != "prov-sess-A" {
		t.Errorf("linked session: got %q", result.LinkedSessionID)
	}
}

// --- LinkSession: force-move preserves cross-repo coverage ---

func TestLinkSession_ForceMove_PreservesCrossRepo(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Also create a second repo with the same provider session (cross-repo).
	repoPath2 := filepath.Join(dir, "repos", "sdk")
	semDir2 := filepath.Join(repoPath2, ".semantica")
	_ = os.MkdirAll(semDir2, 0o755)
	dbPath2 := filepath.Join(semDir2, "lineage.db")
	repoH2, _ := sqlstore.Open(ctx, dbPath2, sqlstore.DefaultOpenOptions())
	repoID2 := uuid.NewString()
	_ = repoH2.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID2, RootPath: repoPath2, CreatedAt: now, EnabledAt: now,
	})
	src2, _ := repoH2.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID2,
		Provider: "claude_code", SourceKey: "src-2", LastSeenAt: now, CreatedAt: now,
	})
	sessSDK, _ := repoH2.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-A",
		RepositoryID: repoID2, Provider: "claude_code", SourceID: src2.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_ = sqlstore.Close(repoH2)

	// Create old implementation with the session in both repos.
	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	oldImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: oldImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", CanonicalPath: repoPath,
		SessionID: sessions[0].localID, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", CanonicalPath: repoPath2,
		SessionID: sessSDK.SessionID, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		DisplayName: "api", RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath2,
		DisplayName: "sdk", RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now,
	})

	// Create new implementation to move into.
	newImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: newImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	// Force-move the session.
	result, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: newImplID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
		Force:            true,
	})
	if err != nil {
		t.Fatalf("force-move: %v", err)
	}
	if result.MovedFrom != oldImplID {
		t.Errorf("expected movedFrom=%s, got %q", oldImplID[:8], result.MovedFrom)
	}

	// Verify: new implementation has BOTH repo sessions (cross-repo preserved).
	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	newRepoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, newImplID)
	if len(newRepoSess) != 2 {
		t.Fatalf("expected 2 repo sessions in target (cross-repo), got %d", len(newRepoSess))
	}

	newRepos, _ := h.Queries.ListImplementationRepos(ctx, newImplID)
	if len(newRepos) != 2 {
		t.Fatalf("expected 2 repos in target, got %d", len(newRepos))
	}

	// Verify: old implementation has NO sessions or repos left.
	oldProvSess, _ := h.Queries.ListProviderSessionsForImplementation(ctx, oldImplID)
	if len(oldProvSess) != 0 {
		t.Errorf("old impl should have 0 provider sessions, got %d", len(oldProvSess))
	}
	oldRepoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, oldImplID)
	if len(oldRepoSess) != 0 {
		t.Errorf("old impl should have 0 repo sessions, got %d", len(oldRepoSess))
	}
	oldRepos, _ := h.Queries.ListImplementationRepos(ctx, oldImplID)
	if len(oldRepos) != 0 {
		t.Errorf("old impl should have 0 repos (orphaned cleaned), got %d", len(oldRepos))
	}
}

// --- LinkSession: force-move without --force returns error ---

func TestLinkSession_MoveWithoutForce_Errors(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())

	oldImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: oldImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})

	newImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: newImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: newImplID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
		Force:            false,
	})
	if err == nil {
		t.Fatal("expected error when session is owned without --force")
	}
}

// --- ForceMove: orphan cleanup preserves repo with commits ---

func TestLinkSession_ForceMove_PreservesRepoWithCommits(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())

	oldImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: oldImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", CanonicalPath: repoPath,
		SessionID: sessions[0].localID, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		DisplayName: "api", RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	// Repo also has a commit — should survive orphan cleanup even after
	// sessions are moved out.
	_ = h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		CommitHash: "abc123", AttachedAt: now, AttachRule: "session_identity",
	})

	newImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: newImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: newImplID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
		Force:            true,
	})
	if err != nil {
		t.Fatalf("force-move: %v", err)
	}

	// Old implementation lost its sessions but still has a commit for that repo.
	// Orphan cleanup should preserve the repo membership.
	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	oldRepos, _ := h.Queries.ListImplementationRepos(ctx, oldImplID)
	if len(oldRepos) != 1 {
		t.Errorf("old impl should keep repo (has commit), got %d repos", len(oldRepos))
	}

	oldCommits, _ := h.Queries.ListImplementationCommits(ctx, oldImplID)
	if len(oldCommits) != 1 {
		t.Errorf("old impl should keep commit, got %d", len(oldCommits))
	}
}

// --- Ambiguity error propagation ---

func TestLinkSession_AmbiguousProviderSessionID_Errors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create implementations.db.
	implPath := filepath.Join(dir, "implementations.db")
	implH, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = implH.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(implH)

	// Create a repo where two providers share the same provider_session_id.
	repoPath := filepath.Join(dir, "repos", "api")
	semDir := filepath.Join(repoPath, ".semantica")
	_ = os.MkdirAll(semDir, 0o755)
	dbPath := filepath.Join(semDir, "lineage.db")
	repoH, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoID := uuid.NewString()
	_ = repoH.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: now, EnabledAt: now,
	})

	srcA, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-a", LastSeenAt: now, CreatedAt: now,
	})
	srcB, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "cursor", SourceKey: "src-b", LastSeenAt: now, CreatedAt: now,
	})

	// Same provider_session_id, different providers.
	_, _ = repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "ambiguous-id",
		RepositoryID: repoID, Provider: "claude_code", SourceID: srcA.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_, _ = repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "ambiguous-id",
		RepositoryID: repoID, Provider: "cursor", SourceID: srcB.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_ = sqlstore.Close(repoH)

	// Try to link by the ambiguous provider_session_id.
	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        "ambiguous-id",
		RepoPath:         repoPath,
	})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguity error, got: %v", err)
	}
}

// --- Idempotent: linking to same target is a no-op ---

func TestLinkSession_AlreadyLinkedToTarget_Idempotent(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	// Pre-attach the session to the target.
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: implID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = impldb.Close(h)

	// Link the same session again — should succeed without error.
	result, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("idempotent link should not error: %v", err)
	}
	if result.MovedFrom != "" {
		t.Errorf("expected no move, got movedFrom=%q", result.MovedFrom)
	}
}

// --- Fresh link: repo not in broker registry, resolved session is baseline ---

func TestLinkSession_RepoNotInBroker_StillLinksResolvedSlice(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Deliberately do NOT register repoPath in broker.

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	result, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("link with unregistered repo: %v", err)
	}
	if result.LinkedProvider != "claude_code" {
		t.Errorf("provider: got %q", result.LinkedProvider)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	repoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(repoSess) != 1 {
		t.Errorf("expected 1 repo session (baseline fallback), got %d", len(repoSess))
	}

	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if len(repos) != 1 {
		t.Errorf("expected 1 repo (baseline fallback), got %d", len(repos))
	}
}

// --- Idempotent with backfill: re-link fills missing repo slices ---

func TestLinkSession_IdempotentBackfillsMissingSlices(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Register repo in broker.
	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	// Create implementation with provider session but NO repo sessions.
	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: implID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = impldb.Close(h)

	// Verify: no repo sessions yet.
	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	repoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(repoSess) != 0 {
		t.Fatalf("precondition: expected 0 repo sessions, got %d", len(repoSess))
	}
	_ = impldb.Close(h)

	// Re-link — should backfill the missing repo session.
	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("idempotent re-link: %v", err)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	repoSess, _ = h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(repoSess) != 1 {
		t.Errorf("expected 1 repo session after backfill, got %d", len(repoSess))
	}
	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if len(repos) != 1 {
		t.Errorf("expected 1 repo after backfill, got %d", len(repos))
	}
}

// --- Fresh link: captures all repo slices of a cross-repo session ---

func TestLinkSession_FreshLink_AllRepoSlices(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create a second repo with the same provider session (cross-repo).
	repoPath2 := filepath.Join(dir, "repos", "sdk")
	semDir2 := filepath.Join(repoPath2, ".semantica")
	_ = os.MkdirAll(semDir2, 0o755)
	dbPath2 := filepath.Join(semDir2, "lineage.db")
	repoH2, _ := sqlstore.Open(ctx, dbPath2, sqlstore.DefaultOpenOptions())
	repoID2 := uuid.NewString()
	_ = repoH2.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID2, RootPath: repoPath2, CreatedAt: now, EnabledAt: now,
	})
	src2, _ := repoH2.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID2,
		Provider: "claude_code", SourceKey: "src-2", LastSeenAt: now, CreatedAt: now,
	})
	_, _ = repoH2.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-sess-A",
		RepositoryID: repoID2, Provider: "claude_code", SourceID: src2.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_ = sqlstore.Close(repoH2)

	// Register both repos in the broker so findAllRepoSlices can find them.
	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Register(ctx, bh, repoPath2, repoPath2)
	_ = broker.Close(bh)

	// Create implementation and do a fresh link.
	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("fresh link: %v", err)
	}

	// Verify: both repo slices were captured.
	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	repoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(repoSess) != 2 {
		t.Errorf("expected 2 repo sessions (cross-repo), got %d", len(repoSess))
	}

	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(repos))
	}
}

// --- Force-move cleans orphaned branches from source ---

func TestLinkSession_ForceMove_CleansOrphanedBranches(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())

	oldImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: oldImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", CanonicalPath: repoPath,
		SessionID: sessions[0].localID, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		DisplayName: "api", RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		Branch: "feature/auth", FirstSeenAt: now, LastSeenAt: now,
	})

	newImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: newImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: newImplID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
		Force:            true,
	})
	if err != nil {
		t.Fatalf("force-move: %v", err)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	// Old implementation should have no branches left.
	oldBranches, _ := h.Queries.ListBranchesForImplementation(ctx, oldImplID)
	if len(oldBranches) != 0 {
		t.Errorf("old impl should have 0 branches after move, got %d", len(oldBranches))
	}
}

// --- Force-move preserves repo roles from source ---

func TestLinkSession_ForceMove_PreservesRepoRoles(t *testing.T) {
	dir, repoPath, sessions := setupLinkEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())

	oldImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: oldImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: oldImplID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-A", CanonicalPath: repoPath,
		SessionID: sessions[0].localID, FirstSeenAt: now, LastSeenAt: now,
	})
	// Repo has origin role in the old implementation.
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: oldImplID, CanonicalPath: repoPath,
		DisplayName: "api", RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})

	newImplID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: newImplID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: newImplID,
		SessionID:        sessions[0].localID,
		RepoPath:         repoPath,
		Force:            true,
	})
	if err != nil {
		t.Fatalf("force-move: %v", err)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	// Target should have the repo with origin role (carried from source).
	repos, _ := h.Queries.ListImplementationRepos(ctx, newImplID)
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0].RepoRole != "origin" {
		t.Errorf("expected origin role carried from source, got %q", repos[0].RepoRole)
	}
}

// --- Fresh link writes branch rows into target ---

func TestLinkSession_FreshLink_WritesBranch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create a git repo so GitDetectBranch returns something.
	repoPath := filepath.Join(dir, "repos", "api")
	_ = os.MkdirAll(repoPath, 0o755)
	exec.Command("git", "init", repoPath).Run()
	exec.Command("git", "-C", repoPath, "checkout", "-b", "feature/test-branch").Run()
	os.WriteFile(filepath.Join(repoPath, ".gitkeep"), nil, 0o644)
	exec.Command("git", "-C", repoPath, "add", ".").Run()
	exec.Command("git", "-C", repoPath, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-m", "init").Run()
	// Create .semantica with lineage.db.
	semDir := filepath.Join(repoPath, ".semantica")
	_ = os.MkdirAll(semDir, 0o755)
	dbPath := filepath.Join(semDir, "lineage.db")
	repoH, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoID := uuid.NewString()
	_ = repoH.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: now, EnabledAt: now,
	})
	src, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src", LastSeenAt: now, CreatedAt: now,
	})
	sess, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-branch-test",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_ = sqlstore.Close(repoH)

	// Register in broker.
	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	// Create implementation.
	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	// Fresh link.
	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sess.SessionID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	branches, _ := h.Queries.ListBranchesForImplementation(ctx, implID)
	if len(branches) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(branches))
	}
	if branches[0].Branch != "feature/test-branch" {
		t.Errorf("expected branch feature/test-branch, got %q", branches[0].Branch)
	}
}

// --- Idempotent backfill writes branch rows ---

func TestLinkSession_IdempotentBackfill_WritesBranch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create a git repo.
	repoPath := filepath.Join(dir, "repos", "api")
	_ = os.MkdirAll(repoPath, 0o755)
	exec.Command("git", "init", repoPath).Run()
	exec.Command("git", "-C", repoPath, "checkout", "-b", "feature/backfill").Run()
	os.WriteFile(filepath.Join(repoPath, ".gitkeep"), nil, 0o644)
	exec.Command("git", "-C", repoPath, "add", ".").Run()
	exec.Command("git", "-C", repoPath, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "-m", "init").Run()
	semDir := filepath.Join(repoPath, ".semantica")
	_ = os.MkdirAll(semDir, 0o755)
	dbPath := filepath.Join(semDir, "lineage.db")
	repoH, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoID := uuid.NewString()
	_ = repoH.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: now, EnabledAt: now,
	})
	src, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src", LastSeenAt: now, CreatedAt: now,
	})
	sess, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-backfill-test",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now, LastSeenAt: now,
	})
	_ = sqlstore.Close(repoH)

	registryPath, _ := broker.DefaultRegistryPath()
	bh, _ := broker.Open(ctx, registryPath)
	_ = broker.Register(ctx, bh, repoPath, repoPath)
	_ = broker.Close(bh)

	// Create implementation with provider session but NO branch rows.
	implPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	implID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implID, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: implID, Provider: "claude_code",
		ProviderSessionID: "prov-backfill-test", AttachRule: "new", AttachedAt: now,
	})
	_ = impldb.Close(h)

	// Re-link triggers idempotent backfill.
	_, err := LinkSession(ctx, LinkSessionInput{
		ImplementationID: implID,
		SessionID:        sess.SessionID,
		RepoPath:         repoPath,
	})
	if err != nil {
		t.Fatalf("backfill link: %v", err)
	}

	h, _ = impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	branches, _ := h.Queries.ListBranchesForImplementation(ctx, implID)
	if len(branches) != 1 {
		t.Fatalf("expected 1 branch after backfill, got %d", len(branches))
	}
	if branches[0].Branch != "feature/backfill" {
		t.Errorf("expected branch feature/backfill, got %q", branches[0].Branch)
	}
}

// --- Repo role upsert never downgrades downstream to related ---

func TestRepoUpsert_NeverDowngradesDownstream(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: id, CanonicalPath: "/repos/sdk",
		DisplayName: "sdk", RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now,
	})

	// Upsert with "related" should NOT downgrade.
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: id, CanonicalPath: "/repos/sdk",
		DisplayName: "sdk", RepoRole: "related", FirstSeenAt: now, LastSeenAt: now + 1000,
	})

	repos, _ := h.Queries.ListImplementationRepos(ctx, id)
	if repos[0].RepoRole != "downstream" {
		t.Errorf("expected downstream preserved, got %q", repos[0].RepoRole)
	}
}

func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
