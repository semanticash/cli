package impldb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// openTestDB creates a temp implementations database with migrations applied.
func openTestDB(t *testing.T) *Handle {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "implementations.db")

	h, err := Open(ctx, dbPath, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = Close(h) })
	return h
}

func insertImpl(t *testing.T, ctx context.Context, q *impldbgen.Queries, state string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	if err := q.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		var closedAt sql.NullInt64
		if state == "closed" {
			closedAt = NullInt64(now)
		}
		if err := q.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
			State: state, ClosedAt: closedAt, ImplementationID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// --- Migration tests ---

func TestMigrate_AppliesCleanly(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	h, err := Open(ctx, dbPath, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = Close(h)

	// Second run is a no-op (no error).
	h, err = Open(ctx, dbPath, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	_ = Close(h)
}

// --- Implementation CRUD ---

func TestInsertAndGetImplementation(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	id := uuid.NewString()

	err := h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	impl, err := h.Queries.GetImplementation(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if impl.ImplementationID != id {
		t.Errorf("got id %q, want %q", impl.ImplementationID, id)
	}
	if impl.State != "active" {
		t.Errorf("got state %q, want active", impl.State)
	}
	if impl.Title.Valid {
		t.Errorf("expected null title, got %q", impl.Title.String)
	}
}

// --- Observation lifecycle ---

func TestObservation_InsertListReconcile(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	obsID := uuid.NewString()

	err := h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     obsID,
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
		TargetRepoPath:    "/repos/api",
		EventTs:           now,
		CreatedAt:         now,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	pending, err := h.Queries.ListPendingObservations(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1", len(pending))
	}
	if pending[0].Provider != "claude_code" {
		t.Errorf("got provider %q", pending[0].Provider)
	}

	err = h.Queries.MarkObservationReconciled(ctx, obsID)
	if err != nil {
		t.Fatalf("mark reconciled: %v", err)
	}

	pending2, err := h.Queries.ListPendingObservations(ctx, 10)
	if err != nil {
		t.Fatalf("list pending after reconcile: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("got %d pending after reconcile, want 0", len(pending2))
	}
}

func TestObservation_FailedAndRetry(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	obsID := uuid.NewString()

	_ = h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     obsID,
		Provider:          "claude_code",
		ProviderSessionID: "sess-fail",
		TargetRepoPath:    "/repos/api",
		EventTs:           now,
		CreatedAt:         now,
	})

	_ = h.Queries.MarkObservationFailed(ctx, impldbgen.MarkObservationFailedParams{
		LastError:     NullStr("test error"),
		ObservationID: obsID,
	})

	retryable, _ := h.Queries.ListRetryableObservations(ctx, impldbgen.ListRetryableObservationsParams{
		ReconcileAttempts: 3,
		Limit:             10,
	})
	if len(retryable) != 1 {
		t.Fatalf("got %d retryable, want 1", len(retryable))
	}
	if retryable[0].ReconcileAttempts != 1 {
		t.Errorf("got attempts %d, want 1", retryable[0].ReconcileAttempts)
	}
}

func TestObservation_DeferredState(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	obsID := uuid.NewString()

	_ = h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     obsID,
		Provider:          "claude_code",
		ProviderSessionID: "sess-child",
		ParentSessionID:   NullStr("sess-parent"),
		TargetRepoPath:    "/repos/api",
		EventTs:           now,
		CreatedAt:         now,
	})

	_ = h.Queries.MarkObservationDeferred(ctx, obsID)

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("deferred should not appear in pending, got %d", len(pending))
	}

	deferred, _ := h.Queries.ListDeferredObservations(ctx, 10)
	if len(deferred) != 1 {
		t.Fatalf("got %d deferred, want 1", len(deferred))
	}
}

func TestObservation_PendingSortsParentsFirst(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Insert child first (has parent_session_id)
	_ = h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     uuid.NewString(),
		Provider:          "claude_code",
		ProviderSessionID: "child-sess",
		ParentSessionID:   NullStr("parent-sess"),
		TargetRepoPath:    "/repos/api",
		EventTs:           now,
		CreatedAt:         now,
	})

	// Insert parent second (no parent_session_id)
	_ = h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     uuid.NewString(),
		Provider:          "claude_code",
		ProviderSessionID: "parent-sess",
		TargetRepoPath:    "/repos/api",
		EventTs:           now,
		CreatedAt:         now + 1,
	})

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 2 {
		t.Fatalf("got %d pending, want 2", len(pending))
	}

	if pending[0].ProviderSessionID != "parent-sess" {
		t.Errorf("expected parent first, got %q", pending[0].ProviderSessionID)
	}
	if pending[1].ProviderSessionID != "child-sess" {
		t.Errorf("expected child second, got %q", pending[1].ProviderSessionID)
	}
}

// --- Provider session: same session can span multiple implementations ---

func TestProviderSession_SameSessionMultipleImplementations(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implA := insertImpl(t, ctx, h.Queries, "active")

	// Attach session to implA
	err := h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  implA,
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
		AttachRule:        "new",
		AttachedAt:        now,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Owner query should find implA (active)
	owner, err := h.Queries.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	if owner != implA {
		t.Errorf("got owner %q, want %q", owner, implA)
	}

	// Close implA
	_ = h.Queries.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State: "closed", ClosedAt: NullInt64(now), ImplementationID: implA,
	})

	// Owner query should now find nothing (implA is closed)
	_, err = h.Queries.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
	})
	if err == nil {
		t.Fatal("expected no owner after close, but got one")
	}

	// Same session can now attach to a new implementation
	implB := insertImpl(t, ctx, h.Queries, "active")
	err = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  implB,
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
		AttachRule:        "branch_active",
		AttachedAt:        now + 1000,
	})
	if err != nil {
		t.Fatalf("second insert should succeed after close: %v", err)
	}

	// Owner should now be implB
	owner2, err := h.Queries.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("get owner after reattach: %v", err)
	}
	if owner2 != implB {
		t.Errorf("got owner %q, want %q", owner2, implB)
	}
}

func TestProviderSession_FindFiltersClosedImplementations(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	closedImpl := insertImpl(t, ctx, h.Queries, "closed")
	activeImpl := insertImpl(t, ctx, h.Queries, "active")

	// Same session in both
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: closedImpl, Provider: "claude_code",
		ProviderSessionID: "sess-1", AttachRule: "new", AttachedAt: now,
	})
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: activeImpl, Provider: "claude_code",
		ProviderSessionID: "sess-1", AttachRule: "branch_active", AttachedAt: now + 1000,
	})

	// FindImplementationByProviderSession should return the active one
	impl, err := h.Queries.FindImplementationByProviderSession(ctx, impldbgen.FindImplementationByProviderSessionParams{
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if impl.ImplementationID != activeImpl {
		t.Errorf("got %q, want %q (active)", impl.ImplementationID, activeImpl)
	}
}

// --- Repo role: origin never downgrades ---

func TestRepoRole_OriginNeverDowngrades(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implID := insertImpl(t, ctx, h.Queries, "active")

	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})

	// Try to downgrade to downstream
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now + 1000,
	})

	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].RepoRole != "origin" {
		t.Errorf("role was downgraded to %q, should remain origin", repos[0].RepoRole)
	}
	if repos[0].LastSeenAt != now+1000 {
		t.Errorf("last_seen_at should have updated")
	}
}

func TestRepoRole_CanPromoteToOrigin(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implID := insertImpl(t, ctx, h.Queries, "active")

	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now,
	})

	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now + 1000,
	})

	repos, _ := h.Queries.ListImplementationRepos(ctx, implID)
	if repos[0].RepoRole != "origin" {
		t.Errorf("expected promotion to origin, got %q", repos[0].RepoRole)
	}
}

// --- Branch upsert preserves first_seen_at ---

func TestBranch_UpsertPreservesFirstSeen(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implID := insertImpl(t, ctx, h.Queries, "active")

	_ = h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
		ImplementationID: implID, CanonicalPath: "/repos/api",
		Branch: "feature/auth", FirstSeenAt: now, LastSeenAt: now,
	})

	_ = h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
		ImplementationID: implID, CanonicalPath: "/repos/api",
		Branch: "feature/auth", FirstSeenAt: now + 5000, LastSeenAt: now + 5000,
	})

	branches, _ := h.Queries.ListBranchesForImplementation(ctx, implID)
	if len(branches) != 1 {
		t.Fatalf("got %d branches, want 1", len(branches))
	}
	if branches[0].FirstSeenAt != now {
		t.Errorf("first_seen_at changed from %d to %d", now, branches[0].FirstSeenAt)
	}
	if branches[0].LastSeenAt != now+5000 {
		t.Errorf("last_seen_at should have updated")
	}
}

// --- Commit partial unique index ---

func TestCommit_AutoUniqueIndex(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implA := insertImpl(t, ctx, h.Queries, "active")
	implB := insertImpl(t, ctx, h.Queries, "active")

	// First auto-attach succeeds
	err := h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: implA, CanonicalPath: "/repos/api",
		CommitHash: "abc123", AttachedAt: now, AttachRule: "session_identity",
	})
	if err != nil {
		t.Fatalf("first commit insert: %v", err)
	}

	// Second auto-attach: silently ignored by INSERT OR IGNORE + partial unique index
	err = h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: implB, CanonicalPath: "/repos/api",
		CommitHash: "abc123", AttachedAt: now, AttachRule: "session_identity",
	})
	if err != nil {
		t.Fatalf("second commit insert should not error: %v", err)
	}

	owner, err := h.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: "/repos/api", CommitHash: "abc123",
	})
	if err != nil {
		t.Fatalf("find by commit: %v", err)
	}
	if owner != implA {
		t.Errorf("got owner %q, want %q", owner, implA)
	}

	// explicit_link to second implementation should work
	err = h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: implB, CanonicalPath: "/repos/api",
		CommitHash: "abc123", AttachedAt: now, AttachRule: "explicit_link",
	})
	if err != nil {
		t.Fatalf("explicit_link commit insert should succeed: %v", err)
	}
}

// --- Dormancy ---

func TestMarkDormant(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	old := now - 3600_000

	activeID := insertImpl(t, ctx, h.Queries, "active")
	// Insert stale one manually to control last_activity_at
	staleID := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: staleID, CreatedAt: old, LastActivityAt: old,
	})

	threshold := now - 1800_000
	result, err := h.Queries.MarkDormant(ctx, threshold)
	if err != nil {
		t.Fatalf("mark dormant: %v", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		t.Errorf("expected 1 affected, got %d", affected)
	}

	impl, _ := h.Queries.GetImplementation(ctx, staleID)
	if impl.State != "dormant" {
		t.Errorf("expected dormant, got %q", impl.State)
	}

	active, _ := h.Queries.GetImplementation(ctx, activeID)
	if active.State != "active" {
		t.Errorf("expected active, got %q", active.State)
	}
}

// --- Repo sessions (multi-repo per provider session) ---

func TestRepoSessions_MultiRepo(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implID := insertImpl(t, ctx, h.Queries, "active")

	localA := uuid.NewString()
	localB := uuid.NewString()

	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: implID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-1", CanonicalPath: "/repos/api",
		SessionID: localA, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: implID, Provider: "claude_code",
		ProviderSessionID: "prov-sess-1", CanonicalPath: "/repos/sdk",
		SessionID: localB, FirstSeenAt: now, LastSeenAt: now,
	})

	sessions, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)
	if len(sessions) != 2 {
		t.Fatalf("got %d repo sessions, want 2", len(sessions))
	}

	// Look up by local session ID - returns all matching implementations
	found, err := h.Queries.FindImplementationsByLocalSession(ctx, localB)
	if err != nil {
		t.Fatalf("find by local session: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d implementations, want 1", len(found))
	}
	if found[0].ImplementationID != implID {
		t.Errorf("got impl %q, want %q", found[0].ImplementationID, implID)
	}
}

func TestRepoSessions_LocalSessionMultipleImplementations(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	implA := insertImpl(t, ctx, h.Queries, "closed")
	implB := insertImpl(t, ctx, h.Queries, "active")
	localID := uuid.NewString()

	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: implA, Provider: "claude_code",
		ProviderSessionID: "prov-sess-1", CanonicalPath: "/repos/api",
		SessionID: localID, FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
		ImplementationID: implB, Provider: "claude_code",
		ProviderSessionID: "prov-sess-1", CanonicalPath: "/repos/api",
		SessionID: localID, FirstSeenAt: now + 1000, LastSeenAt: now + 1000,
	})

	// Should return both, ordered by last_activity_at desc (active first)
	found, _ := h.Queries.FindImplementationsByLocalSession(ctx, localID)
	if len(found) != 2 {
		t.Fatalf("got %d implementations, want 2", len(found))
	}
	if found[0].ImplementationID != implB {
		t.Errorf("expected active implementation first, got %q", found[0].State)
	}
}

// Ensure NullStr is used correctly with generated code
var _ sql.NullString = NullStr("test")
