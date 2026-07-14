package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestCheckRepoState_SemDirMissing(t *testing.T) {
	dir := t.TempDir()
	res := CheckRepoState(context.Background(), filepath.Join(dir, "no-such-repo"))
	if res.Verdict != RepoStateStale {
		t.Fatalf("Verdict = %v, want Stale", res.Verdict)
	}
	if res.Reason != RepoStaleSemDirMissing {
		t.Errorf("Reason = %q, want %q", res.Reason, RepoStaleSemDirMissing)
	}
}

func TestCheckRepoState_LineageDBMissing(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := CheckRepoState(context.Background(), repo)
	if res.Verdict != RepoStateStale {
		t.Fatalf("Verdict = %v, want Stale", res.Verdict)
	}
	if res.Reason != RepoStaleLineageDBMissing {
		t.Errorf("Reason = %q, want %q", res.Reason, RepoStaleLineageDBMissing)
	}
}

func TestCheckRepoState_SettingsDisabled(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(context.Background(), dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Deliberately omit the enabled marker to simulate `semantica disable`.

	res := CheckRepoState(context.Background(), repo)
	if res.Verdict != RepoStateStale {
		t.Fatalf("Verdict = %v, want Stale", res.Verdict)
	}
	if res.Reason != RepoStaleSettingsDisabled {
		t.Errorf("Reason = %q, want %q", res.Reason, RepoStaleSettingsDisabled)
	}
}

func TestCheckRepoState_NoRepoRow(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(context.Background(), dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// DB is migrated but no repository row is inserted.

	res := CheckRepoState(context.Background(), repo)
	if res.Verdict != RepoStateStale {
		t.Fatalf("Verdict = %v, want Stale", res.Verdict)
	}
	if res.Reason != RepoStaleNoRepoRow {
		t.Errorf("Reason = %q, want %q", res.Reason, RepoStaleNoRepoRow)
	}
}

func TestCheckRepoState_OK(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	ctx := context.Background()
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(),
		RootPath:     repo,
		CreatedAt:    1000,
		EnabledAt:    1000,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	res := CheckRepoState(ctx, repo)
	if res.Verdict != RepoStateOK {
		t.Errorf("Verdict = %v, want OK (Reason=%q, Err=%v)", res.Verdict, res.Reason, res.Err)
	}
}

// A stat error that is not ErrNotExist must not be treated as Stale;
// pruning on transient IO would delete healthy entries. On platforms
// where a chmod-0000 directory still exists, os.Stat succeeds, so the
// check must fall through to the next verdict rather than returning
// Unknown. Skip on windows where chmod semantics differ.
func TestCheckRepoState_UnknownVerdictDoesNotPrune(t *testing.T) {
	// Simulate the Unknown path by pointing at a lineage.db that fails
	// to open: pre-migration write of a garbage file at the DB path.
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "lineage.db"), []byte("not-a-sqlite-file"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := CheckRepoState(context.Background(), repo)
	if res.Verdict != RepoStateUnknown {
		t.Errorf("Verdict = %v (Reason=%q, Err=%v), want Unknown for corrupt DB", res.Verdict, res.Reason, res.Err)
	}
}

// Marker stat errors should produce Unknown, not settings-disabled.
func TestCheckRepoState_MarkerStatError_IsUnknown(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "lineage.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// os.Stat follows this symlink and returns ELOOP.
	markerPath := filepath.Join(semDir, "enabled")
	if err := os.Symlink(markerPath, markerPath); err != nil {
		t.Skipf("cannot create symlink loop: %v", err)
	}

	res := CheckRepoState(context.Background(), repo)
	if res.Verdict == RepoStateStale && res.Reason == RepoStaleSettingsDisabled {
		t.Errorf("marker stat error returned settings-disabled; want Unknown. Err=%v", res.Err)
	}
	if res.Verdict != RepoStateUnknown {
		t.Errorf("Verdict = %v, want Unknown (Err=%v)", res.Verdict, res.Err)
	}
}

func TestPruneStale_RemovesOnlyMatchingCanonicalPaths(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	regPath := filepath.Join(home, "repos.json")

	h, err := Open(ctx, regPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := Register(ctx, h, "/a", "/canon/a"); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := Register(ctx, h, "/b", "/canon/b"); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if err := Register(ctx, h, "/c", "/canon/c"); err != nil {
		t.Fatalf("register c: %v", err)
	}

	stale := map[string]RepoStateReason{
		"/canon/a": RepoStaleSemDirMissing,
		"/canon/c": RepoStaleNoRepoRow,
	}
	removed, err := PruneStale(ctx, h, stale)
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	// Reopen to confirm persistence.
	h2, err := Open(ctx, regPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	repos, err := ListAllRepos(ctx, h2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(repos) != 1 || repos[0].CanonicalPath != "/canon/b" {
		t.Errorf("survivors = %+v, want [/canon/b]", repos)
	}
}
