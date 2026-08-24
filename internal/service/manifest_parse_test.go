package service

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// validCommitManifestBytes returns a valid commit-scoped version-2 manifest.
func validCommitManifestBytes(t *testing.T) []byte {
	t.Helper()
	sha1 := strings.Repeat("a", 40)
	cas := strings.Repeat("c", 64)
	m := blobs.Manifest{
		Version: 2, Scope: blobs.ScopeCommit, ObjectFormat: blobs.ObjectFormatSHA1,
		CommitHash: sha1, TreeID: sha1,
		Files: []blobs.ManifestFile{
			{Path: "a.go", Blob: cas, Size: 3, EntryType: blobs.EntryRegular, GitMode: "100644", GitObjectID: sha1},
		},
	}
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// invalidCommitManifestBytes includes a file but omits required commit identity.
func invalidCommitManifestBytes(t *testing.T) []byte {
	t.Helper()
	sha1 := strings.Repeat("a", 40)
	cas := strings.Repeat("c", 64)
	m := blobs.Manifest{
		Version: 2, Scope: blobs.ScopeCommit, // object_format / commit_hash / tree_id intentionally absent
		Files: []blobs.ManifestFile{
			{Path: "a.go", Blob: cas, Size: 3, EntryType: blobs.EntryRegular, GitMode: "100644", GitObjectID: sha1},
		},
	}
	b, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestLoadManifestForCheckpoint_ParsesV2 verifies strict manifest parsing.
func TestLoadManifestForCheckpoint_ParsesV2(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := t.TempDir()

	hash, _, err := bs.Put(ctx, validCommitManifestBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	files := loadManifestForCheckpoint(ctx, bs, sql.NullString{String: hash, Valid: true}, semDir)
	if len(files) != 1 || files[0].Path != "a.go" {
		t.Errorf("loadManifestForCheckpoint = %+v, want the a.go entry", files)
	}

	badHash, _, err := bs.Put(ctx, invalidCommitManifestBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := loadManifestForCheckpoint(ctx, bs, sql.NullString{String: badHash, Valid: true}, semDir); got != nil {
		t.Errorf("loadManifestForCheckpoint = %+v, want nil for an invalid v2 manifest", got)
	}
}

// TestLoadPreviousManifest_ParsesV2 rejects invalid previous manifests.
func TestLoadPreviousManifest_ParsesV2(t *testing.T) {
	ctx := context.Background()
	h := testDB(t)
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	insertCP := func(repoID string, manifestBytes []byte) {
		t.Helper()
		hash, _, err := bs.Put(ctx, manifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: uuid.NewString(), RepositoryID: repoID, CreatedAt: 1000, Kind: "auto",
			ManifestHash: sqlstore.NullStr(hash), SizeBytes: sql.NullInt64{Int64: 100, Valid: true},
			Status: "complete", CompletedAt: sql.NullInt64{Int64: 1000, Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Query after the repository's only checkpoint.
	repoValid := insertRepo(t, h, 100_000)
	insertCP(repoValid, validCommitManifestBytes(t))
	if res := loadPreviousManifest(ctx, h, bs, repoValid, 1<<30); !res.ok || len(res.files) != 1 || res.files[0].Path != "a.go" {
		t.Errorf("valid v2: ok=%v files=%+v, want ok with the a.go entry", res.ok, res.files)
	}

	repoInvalid := insertRepo(t, h, 100_000)
	insertCP(repoInvalid, invalidCommitManifestBytes(t))
	if res := loadPreviousManifest(ctx, h, bs, repoInvalid, 1<<30); res.ok {
		t.Errorf("invalid v2: ok=true, want false so it cannot seed reuse")
	}
}

// TestShowCheckpoint_ParsesV2 rejects invalid manifests.
func TestShowCheckpoint_ParsesV2(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	gitInit := exec.Command("git", "init", dir)
	gitInit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	semDir := filepath.Join(dir, ".semantica")
	objectsDir := filepath.Join(semDir, "objects")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repo.Root(), CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		t.Fatal(err)
	}
	insertCP := func(manifestBytes []byte) string {
		t.Helper()
		hash, _, err := bs.Put(ctx, manifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.NewString()
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: id, RepositoryID: repoID, CreatedAt: 1, Kind: "auto",
			ManifestHash: sqlstore.NullStr(hash), SizeBytes: sql.NullInt64{Int64: 100, Valid: true},
			Status: "complete", CompletedAt: sql.NullInt64{Int64: 1, Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	validCP := insertCP(validCommitManifestBytes(t))
	invalidCP := insertCP(invalidCommitManifestBytes(t))
	_ = sqlstore.Close(h)

	svc := NewShowService()
	res, err := svc.ShowCheckpoint(ctx, ShowCheckpointInput{RepoPath: dir, CheckpointID: validCP})
	if err != nil {
		t.Fatalf("valid v2 ShowCheckpoint: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "a.go" {
		t.Errorf("ShowCheckpoint files = %+v, want the a.go entry", res.Files)
	}
	if _, err := svc.ShowCheckpoint(ctx, ShowCheckpointInput{RepoPath: dir, CheckpointID: invalidCP}); err == nil {
		t.Error("invalid v2 ShowCheckpoint = nil error, want a parse failure")
	}
}
