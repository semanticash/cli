package implementations

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func openTestDB(t *testing.T) *impldb.Handle {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = impldb.Close(h) })
	return h
}

func insertObs(t *testing.T, ctx context.Context, q *impldbgen.Queries, provider, provSessID, parentSessID, srcPath, targetPath string, ts int64) string {
	t.Helper()
	id := uuid.NewString()
	if err := q.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     id,
		Provider:          provider,
		ProviderSessionID: provSessID,
		ParentSessionID:   impldb.NullStr(parentSessID),
		SourceProjectPath: impldb.NullStr(srcPath),
		TargetRepoPath:    targetPath,
		EventTs:           ts,
		CreatedAt:         ts,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertImpl(t *testing.T, ctx context.Context, q *impldbgen.Queries, state string, lastActivity int64) string {
	t.Helper()
	id := uuid.NewString()
	if err := q.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id,
		CreatedAt:        lastActivity,
		LastActivityAt:   lastActivity,
	}); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		var closedAt sql.NullInt64
		if state == "closed" {
			closedAt = impldb.NullInt64(lastActivity)
		}
		_ = q.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
			State: state, ClosedAt: closedAt, ImplementationID: id,
		})
	}
	return id
}

func newReconciler(branch string) *Reconciler {
	return &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string { return branch },
	}
}

// --- Basic: single observation creates an implementation ---

func TestReconcile_SingleObservation_CreatesImplementation(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// SourceProjectPath matches TargetRepoPath → origin role.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)

	r := newReconciler("main")
	result, err := r.Reconcile(ctx, h)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Processed != 1 {
		t.Errorf("processed: got %d, want 1", result.Processed)
	}

	// Verify implementation was created.
	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1", len(impls))
	}
	if impls[0].State != "active" {
		t.Errorf("state: got %q, want active", impls[0].State)
	}

	// Verify repo was attached with origin role.
	repos, _ := h.Queries.ListImplementationRepos(ctx, impls[0].ImplementationID)
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].RepoRole != "origin" {
		t.Errorf("role: got %q, want origin", repos[0].RepoRole)
	}

	// Verify branch was persisted.
	branches, _ := h.Queries.ListBranchesForImplementation(ctx, impls[0].ImplementationID)
	if len(branches) != 1 || branches[0].Branch != "main" {
		t.Errorf("branch: got %v, want [main]", branches)
	}
}

// --- session_identity: same session attaches to existing implementation ---

func TestReconcile_SessionIdentity_SameSession(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// First observation creates an implementation.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/projects/api", "/repos/api", now)
	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	// Second observation for the same session.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/projects/api", "/repos/api", now+1000)
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1 (same session)", len(impls))
	}
}

// --- session_identity: same session, different repo (cross-repo routing) ---

func TestReconcile_SessionIdentity_CrossRepo(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Session starts in api repo.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	// Same session routes events to sdk repo.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/sdk", now+1000)
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1 (cross-repo)", len(impls))
	}

	repos, _ := h.Queries.ListImplementationRepos(ctx, impls[0].ImplementationID)
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}

	// api is origin (source == target), sdk is downstream.
	roles := map[string]string{}
	for _, rr := range repos {
		roles[rr.DisplayName] = rr.RepoRole
	}
	if roles["api"] != "origin" {
		t.Errorf("api role: got %q, want origin", roles["api"])
	}
	if roles["sdk"] != "downstream" {
		t.Errorf("sdk role: got %q, want downstream", roles["sdk"])
	}
}

// --- session_identity: parent session links child ---

func TestReconcile_SessionIdentity_ParentChild(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Parent observation.
	insertObs(t, ctx, h.Queries, "claude_code", "parent-sess", "", "/repos/api", "/repos/api", now)
	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	// Child observation with parent_session_id.
	insertObs(t, ctx, h.Queries, "claude_code", "child-sess", "parent-sess", "/repos/api", "/repos/api", now+1000)
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1 (child follows parent)", len(impls))
	}
}

// --- branch_active: different session, same branch, active impl ---

func TestReconcile_BranchActive_AttachesToActive(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// First session creates an implementation on feature/auth.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	r := newReconciler("feature/auth")
	_, _ = r.Reconcile(ctx, h)

	// Different session on the same branch.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-2", "", "/repos/api", "/repos/api", now+1000)
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1 (same branch, active)", len(impls))
	}
}

// --- branch_active: does NOT attach to dormant ---

func TestReconcile_BranchActive_DoesNotReviveDormant(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create implementation and make it dormant.
	implID := insertImpl(t, ctx, h.Queries, "dormant", now-3600_000)
	_ = h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
		ImplementationID: implID,
		CanonicalPath:    "/repos/api",
		Branch:           "feature/auth",
		FirstSeenAt:      now - 3600_000,
		LastSeenAt:       now - 3600_000,
	})

	// New session on the same branch.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-new", "", "/repos/api", "/repos/api", now)
	r := newReconciler("feature/auth")
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	// Should create a NEW implementation, not revive the dormant one.
	activeCount := 0
	for _, impl := range impls {
		if impl.State == "active" {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected 1 new active implementation, got %d active out of %d total", activeCount, len(impls))
	}
}

// --- session_identity: CAN revive dormant ---

func TestReconcile_SessionIdentity_RevivesDormant(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Create implementation, attach a session, make it dormant.
	implID := insertImpl(t, ctx, h.Queries, "dormant", now-3600_000)
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  implID,
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
		AttachRule:        "new",
		AttachedAt:        now - 3600_000,
	})

	// Same session sends new activity.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	impl, _ := h.Queries.GetImplementation(ctx, implID)
	if impl.State != "active" {
		t.Errorf("expected dormant→active revival, got %q", impl.State)
	}
}

// --- Dormancy transition ---

func TestReconcile_MarksDormant(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * DormancyTimeout).UnixMilli()

	insertImpl(t, ctx, h.Queries, "active", old)

	r := newReconciler("main")
	result, _ := r.Reconcile(ctx, h)

	if result.MarkedDormant != 1 {
		t.Errorf("expected 1 marked dormant, got %d", result.MarkedDormant)
	}
}

// --- Parent/child deferral ---

func TestReconcile_ChildDeferred_ThenResolved(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Insert child observation first (parent doesn't exist yet).
	insertObs(t, ctx, h.Queries, "claude_code", "child-sess", "parent-sess", "/repos/api", "/repos/api", now)

	r := newReconciler("main")

	// First pass: child is deferred (parent doesn't exist yet).
	result, err := r.Reconcile(ctx, h)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Processed != 0 {
		t.Errorf("pass 1 processed: got %d, want 0 (child should be deferred)", result.Processed)
	}

	// Now insert the parent observation.
	insertObs(t, ctx, h.Queries, "claude_code", "parent-sess", "", "/repos/api", "/repos/api", now)

	// Second pass: parent is pending → processed. Child is in deferred snapshot → resolved.
	result2, _ := r.Reconcile(ctx, h)

	if result2.Processed != 1 {
		t.Errorf("pass 2 processed: got %d, want 1 (parent)", result2.Processed)
	}
	if result2.DeferredResolved != 1 {
		t.Errorf("pass 2 deferred resolved: got %d, want 1 (child)", result2.DeferredResolved)
	}

	// Both should be in the same implementation.
	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1", len(impls))
	}
}

func TestReconcile_ChildFallsThrough_AfterDeferMax(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Insert child with nonexistent parent.
	insertObs(t, ctx, h.Queries, "claude_code", "orphan-child", "nonexistent-parent", "/repos/api", "/repos/api", now)

	r := newReconciler("main")

	// First pass: child is deferred.
	result1, _ := r.Reconcile(ctx, h)
	if result1.Processed != 0 {
		t.Errorf("pass 1: expected 0 processed, got %d", result1.Processed)
	}

	// Second pass: deferred snapshot from step 2 picks up the child.
	// DeferMaxAttempts reached (attempts=1 >= max=1), falls through to standalone.
	result2, _ := r.Reconcile(ctx, h)
	if result2.DeferredResolved != 1 {
		t.Errorf("pass 2 deferred resolved: got %d, want 1 (orphan falls through)", result2.DeferredResolved)
	}

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 1 {
		t.Fatalf("got %d implementations, want 1 (standalone for orphan)", len(impls))
	}
}

// --- Role assignment ---

func TestReconcile_RoleAssignment_OriginVsDownstream(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Origin: SourceProjectPath belongs to target.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	// Downstream: SourceProjectPath is external.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/sdk", now+1000)

	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	repos, _ := h.Queries.ListImplementationRepos(ctx, impls[0].ImplementationID)

	roles := map[string]string{}
	for _, rr := range repos {
		roles[filepath.Base(rr.CanonicalPath)] = rr.RepoRole
	}
	if roles["api"] != "origin" {
		t.Errorf("api should be origin, got %q", roles["api"])
	}
	if roles["sdk"] != "downstream" {
		t.Errorf("sdk should be downstream, got %q", roles["sdk"])
	}
}

func TestReconcile_RoleAssignment_EmptySourceIsOrigin(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Empty SourceProjectPath → treated as origin.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "", "/repos/api", now)

	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	repos, _ := h.Queries.ListImplementationRepos(ctx, impls[0].ImplementationID)
	if repos[0].RepoRole != "origin" {
		t.Errorf("empty source should default to origin, got %q", repos[0].RepoRole)
	}
}

// --- Different sessions, no branch match → separate implementations ---

func TestReconcile_DifferentSessions_NoBranch_SeparateImpls(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	insertObs(t, ctx, h.Queries, "claude_code", "sess-2", "", "/repos/api", "/repos/api", now+1000)

	// No branch → branch_active can't match.
	r := newReconciler("")
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls) != 2 {
		t.Fatalf("got %d implementations, want 2 (no branch, different sessions)", len(impls))
	}
}

// --- Observation marked reconciled after processing ---

func TestReconcile_ObservationMarkedReconciled(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)

	r := newReconciler("main")
	_, _ = r.Reconcile(ctx, h)

	// No pending observations after reconciliation.
	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("got %d pending after reconcile, want 0", len(pending))
	}
}

// --- Session closed then reattaches to new implementation ---

func TestReconcile_SessionReattachesAfterClose(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// First observation creates impl, attach session.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now)
	r := newReconciler("feature/auth")
	_, _ = r.Reconcile(ctx, h)

	impls, _ := h.Queries.ListAllImplementations(ctx, 10)
	firstImplID := impls[0].ImplementationID

	// Close the implementation.
	_ = h.Queries.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State: "closed", ClosedAt: impldb.NullInt64(now), ImplementationID: firstImplID,
	})

	// Same session on a different branch → should create a new implementation.
	insertObs(t, ctx, h.Queries, "claude_code", "sess-1", "", "/repos/api", "/repos/api", now+5000)
	r2 := newReconciler("feature/billing")
	_, _ = r2.Reconcile(ctx, h)

	impls2, _ := h.Queries.ListAllImplementations(ctx, 10)
	if len(impls2) != 2 {
		t.Fatalf("got %d implementations, want 2 (one closed, one new)", len(impls2))
	}

	activeCount := 0
	for _, impl := range impls2 {
		if impl.State == "active" {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected 1 active, got %d", activeCount)
	}
}

// --- AttachCommit: tied session counts skip automatic attachment ---

func TestAttachCommit_TiedSessionCounts_SkipsAttachment(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")

	// Set up a repo with lineage.db, a checkpoint, commit link, and two sessions.
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
	cpID := uuid.NewString()
	_ = repoH.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID, CreatedAt: now,
		Kind: "auto", Status: "complete",
	})
	_ = repoH.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "abc123", RepositoryID: repoID, CheckpointID: cpID, LinkedAt: now,
	})

	// Create sources and sessions for two different providers.
	srcA, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "claude_code", SourceKey: "src-a", LastSeenAt: now, CreatedAt: now,
	})
	srcB, _ := repoH.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID,
		Provider: "cursor", SourceKey: "src-b", LastSeenAt: now, CreatedAt: now,
	})

	sessA, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-A", RepositoryID: repoID,
		Provider: "claude_code", SourceID: srcA.SourceID, StartedAt: now, LastSeenAt: now,
	})
	sessB, _ := repoH.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "prov-B", RepositoryID: repoID,
		Provider: "cursor", SourceID: srcB.SourceID, StartedAt: now, LastSeenAt: now,
	})
	_ = repoH.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
		SessionID: sessA.SessionID, CheckpointID: cpID,
	})
	_ = repoH.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
		SessionID: sessB.SessionID, CheckpointID: cpID,
	})
	_ = sqlstore.Close(repoH)

	// Two implementations, each owning one of the sessions (1 session each = tie).
	implA := insertImpl(t, ctx, h.Queries, "active", now)
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: implA, Provider: "claude_code",
		ProviderSessionID: "prov-A", AttachRule: "new", AttachedAt: now,
	})
	implB := insertImpl(t, ctx, h.Queries, "active", now)
	_ = h.Queries.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID: implB, Provider: "cursor",
		ProviderSessionID: "prov-B", AttachRule: "new", AttachedAt: now,
	})

	// AttachCommit should skip because session counts are tied (1 each).
	r := newReconciler("main")
	_ = r.AttachCommit(ctx, h, AttachCommitInput{
		RepoPath: repoPath, CommitHash: "abc123",
	})

	// Verify no commit was attached.
	_, err = h.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: repoPath, CommitHash: "abc123",
	})
	if err == nil {
		t.Error("expected no commit attachment on tie, but commit was attached")
	}
}
