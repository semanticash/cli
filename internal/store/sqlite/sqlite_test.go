package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// openTestDB creates a temp SQLite database with migrations applied.
func openTestDB(t *testing.T) *Handle {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h, err := Open(ctx, dbPath, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = Close(h) })
	return h
}

// insertTestRepo inserts a repository and returns its ID.
func insertTestRepo(t *testing.T, ctx context.Context, q *sqldb.Queries, rootPath string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	if err := q.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: id,
		RootPath:     rootPath,
		CreatedAt:    now,
		EnabledAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// --- Migration tests ---

func TestMigratePath_AppliesCleanly(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Running again should be a no-op (no error).
	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// --- Open / Close tests ---

func TestOpen_DefaultPragmas(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	// Verify WAL mode.
	var journalMode string
	if err := h.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	// Verify foreign keys are on.
	var fk int
	if err := h.DB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestOpen_EmptyPath(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, "", DefaultOpenOptions())
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

// --- EnsureRepository tests ---

func TestEnsureRepository_Create(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	id, err := EnsureRepository(ctx, h.Queries, "/tmp/test-repo")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Error("expected non-empty repo ID")
	}
}

func TestEnsureRepository_Idempotent(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	id1, err := EnsureRepository(ctx, h.Queries, "/tmp/test-repo")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := EnsureRepository(ctx, h.Queries, "/tmp/test-repo")
	if err != nil {
		t.Fatal(err)
	}

	if id1 != id2 {
		t.Errorf("EnsureRepository not idempotent: %s != %s", id1, id2)
	}
}

func TestEnsureRepository_DifferentPaths(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	id1, _ := EnsureRepository(ctx, h.Queries, "/tmp/repo-a")
	id2, _ := EnsureRepository(ctx, h.Queries, "/tmp/repo-b")

	if id1 == id2 {
		t.Error("different paths should produce different IDs")
	}
}

// --- ResolveCheckpointID tests ---

func TestResolveCheckpointID_FullUUID(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/resolve-test")
	cpID := uuid.NewString()
	now := time.Now().UnixMilli()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		Kind:         "manual",
		Status:       "complete",
		ManifestHash: sql.NullString{String: "fakehash", Valid: true},
		CreatedAt:    now,
		CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveCheckpointID(ctx, h.Queries, repoID, cpID)
	if err != nil {
		t.Fatal(err)
	}
	if got != cpID {
		t.Errorf("resolved = %s, want %s", got, cpID)
	}
}

func TestResolveCheckpointID_Prefix(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/prefix-test")
	cpID := uuid.NewString()
	now := time.Now().UnixMilli()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		Kind:         "manual",
		Status:       "complete",
		ManifestHash: sql.NullString{String: "fakehash", Valid: true},
		CreatedAt:    now,
		CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Use first 8 chars as prefix.
	prefix := cpID[:8]
	got, err := ResolveCheckpointID(ctx, h.Queries, repoID, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got != cpID {
		t.Errorf("resolved = %s, want %s", got, cpID)
	}
}

func TestResolveCheckpointID_NotFound(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/notfound-test")

	_, err := ResolveCheckpointID(ctx, h.Queries, repoID, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent checkpoint")
	}
}

func TestResolveCheckpointID_FullUUID_NotFound(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/fulluuid-notfound")
	fakeUUID := uuid.NewString()

	_, err := ResolveCheckpointID(ctx, h.Queries, repoID, fakeUUID)
	if err == nil {
		t.Fatal("expected error for nonexistent full UUID")
	}
}

// insertTestSource inserts an agent_source and returns its source_id.
func insertTestSource(t *testing.T, ctx context.Context, q *sqldb.Queries, repoID string) string {
	t.Helper()
	srcID := uuid.NewString()
	now := time.Now().UnixMilli()
	row, err := q.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     srcID,
		RepositoryID: repoID,
		Provider:     "test",
		SourceKey:    "/tmp/test-source-" + srcID,
		LastSeenAt:   now,
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.SourceID
}

// --- ResolveSessionID tests ---

func TestResolveSessionID_FullUUID(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/session-test")
	srcID := insertTestSource(t, ctx, h.Queries, repoID)
	sessID := uuid.NewString()
	now := time.Now().UnixMilli()

	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         sessID,
		ProviderSessionID: "prov-123",
		RepositoryID:      repoID,
		Provider:          "test",
		SourceID:          srcID,
		StartedAt:         now,
		LastSeenAt:        now,
		MetadataJson:      "{}",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSessionID(ctx, h.Queries, repoID, sessID)
	if err != nil {
		t.Fatal(err)
	}
	if got != sessID {
		t.Errorf("resolved = %s, want %s", got, sessID)
	}
}

func TestResolveSessionID_NotFound(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/sess-notfound")

	_, err := ResolveSessionID(ctx, h.Queries, repoID, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestResolveSessionID_WrongRepo(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoA := insertTestRepo(t, ctx, h.Queries, "/tmp/repo-a")
	repoB := insertTestRepo(t, ctx, h.Queries, "/tmp/repo-b")

	srcID := insertTestSource(t, ctx, h.Queries, repoA)
	sessID := uuid.NewString()
	now := time.Now().UnixMilli()

	// Insert session for repo A.
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         sessID,
		ProviderSessionID: "prov-456",
		RepositoryID:      repoA,
		Provider:          "test",
		SourceID:          srcID,
		StartedAt:         now,
		LastSeenAt:        now,
		MetadataJson:      "{}",
	}); err != nil {
		t.Fatal(err)
	}

	// Resolving from repo B should fail.
	_, err := ResolveSessionID(ctx, h.Queries, repoB, sessID)
	if err == nil {
		t.Fatal("expected error when resolving session from wrong repo")
	}
}

// --- Open with custom options ---

func TestOpen_CustomSynchronous(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "custom.db")

	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}

	h, err := Open(ctx, dbPath, OpenOptions{
		BusyTimeout: 500 * time.Millisecond,
		Synchronous: "FULL",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Close(h) }()

	var syncMode int
	if err := h.DB.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
		t.Fatal(err)
	}
	// FULL = 2
	if syncMode != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", syncMode)
	}

	var busyTimeout int
	if err := h.DB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 500 {
		t.Errorf("busy_timeout = %d, want 500", busyTimeout)
	}
}

func TestSQLiteDSN_EmbedsBusyTimeoutPragmas(t *testing.T) {
	got := sqliteDSN("/tmp/test.db", OpenOptions{
		BusyTimeout: 500 * time.Millisecond,
		Synchronous: "FULL",
	})

	for _, want := range []string{
		"_pragma=busy_timeout%28500%29",
		"_pragma=journal_mode%28WAL%29",
		"_pragma=foreign_keys%28ON%29",
		"_pragma=synchronous%28FULL%29",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sqliteDSN() = %q, want substring %q", got, want)
		}
	}
}

// --- Ambiguous prefix tests ---

func TestResolveCheckpointID_AmbiguousPrefix(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/ambiguous-cp")
	now := time.Now().UnixMilli()

	// Insert two checkpoints with the same first 8 chars by crafting UUIDs.
	cp1 := "aaaaaaaa-1111-2222-3333-444444444444"
	cp2 := "aaaaaaaa-5555-6666-7777-888888888888"

	for _, cpID := range []string{cp1, cp2} {
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: cpID,
			RepositoryID: repoID,
			Kind:         "manual",
			Status:       "complete",
			ManifestHash: sql.NullString{String: "hash-" + cpID, Valid: true},
			CreatedAt:    now,
			CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Prefix "aaaaaaaa" matches both - should return ambiguous error.
	_, err := ResolveCheckpointID(ctx, h.Queries, repoID, "aaaaaaaa")
	if err == nil {
		t.Fatal("expected error for ambiguous prefix")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguous error, got: %v", err)
	}

	// A longer prefix that uniquely identifies cp1 should succeed.
	got, err := ResolveCheckpointID(ctx, h.Queries, repoID, "aaaaaaaa-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != cp1 {
		t.Errorf("resolved = %s, want %s", got, cp1)
	}
}

func TestResolveSessionID_AmbiguousPrefix(t *testing.T) {
	h := openTestDB(t)
	ctx := context.Background()

	repoID := insertTestRepo(t, ctx, h.Queries, "/tmp/ambiguous-sess")
	srcID := insertTestSource(t, ctx, h.Queries, repoID)
	now := time.Now().UnixMilli()

	sess1 := "bbbbbbbb-1111-2222-3333-444444444444"
	sess2 := "bbbbbbbb-5555-6666-7777-888888888888"

	for i, sessID := range []string{sess1, sess2} {
		if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
			SessionID:         sessID,
			ProviderSessionID: fmt.Sprintf("prov-%d", i),
			RepositoryID:      repoID,
			Provider:          "test",
			SourceID:          srcID,
			StartedAt:         now,
			LastSeenAt:        now,
			MetadataJson:      "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Prefix "bbbbbbbb" matches both - should return ambiguous error.
	_, err := ResolveSessionID(ctx, h.Queries, repoID, "bbbbbbbb")
	if err == nil {
		t.Fatal("expected error for ambiguous prefix")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguous error, got: %v", err)
	}

	// A longer prefix that uniquely identifies sess2 should succeed.
	got, err := ResolveSessionID(ctx, h.Queries, repoID, "bbbbbbbb-5")
	if err != nil {
		t.Fatal(err)
	}
	if got != sess2 {
		t.Errorf("resolved = %s, want %s", got, sess2)
	}
}

// --- NullStr / NullInt64 tests ---

func TestNullStr(t *testing.T) {
	if got := NullStr(""); got.Valid {
		t.Error("empty string should be null")
	}
	if got := NullStr("hello"); !got.Valid || got.String != "hello" {
		t.Errorf("NullStr(hello) = %v", got)
	}
}

func TestNullInt64(t *testing.T) {
	if got := NullInt64(0); got.Valid {
		t.Error("0 should be null")
	}
	if got := NullInt64(-1); got.Valid {
		t.Error("-1 should be null")
	}
	if got := NullInt64(42); !got.Valid || got.Int64 != 42 {
		t.Errorf("NullInt64(42) = %v", got)
	}
}
