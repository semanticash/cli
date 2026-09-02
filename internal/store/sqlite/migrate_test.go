package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func openRaw(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, DefaultOpenOptions()))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrateWithSource(db *sql.DB, fsys fstest.MapFS, target uint) error {
	driver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		return err
	}
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return err
	}
	return m.Migrate(target)
}

// schemaAt returns migrations through maxVersion.
func schemaAt(t *testing.T, maxVersion int) fstest.MapFS {
	t.Helper()
	entries, err := migrationsFS.ReadDir("schema")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err != nil || v > maxVersion {
			continue
		}
		data, err := migrationsFS.ReadFile("schema/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		fsys[e.Name()] = &fstest.MapFile{Data: data}
	}
	return fsys
}

func latestMigrationVersion(t *testing.T) uint {
	t.Helper()
	entries, err := migrationsFS.ReadDir("schema")
	if err != nil {
		t.Fatal(err)
	}
	var latest int
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err == nil && v > latest {
			latest = v
		}
	}
	return uint(latest)
}

func dbVersion(t *testing.T, db *sql.DB) (uint, bool) {
	t.Helper()
	driver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		t.Fatal(err)
	}
	v, dirty, err := driver.Version()
	if err != nil {
		t.Fatal(err)
	}
	return uint(v), dirty
}

func seedV1(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-a', '/tmp/a', 1, 1)`,
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-b', '/tmp/b', 1, 1)`,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status) values ('ck-z-early', 'repo-a', 50, 'auto', 'complete')`,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status) values ('ck-b-tie', 'repo-a', 100, 'auto', 'pending')`,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status) values ('ck-a-tie', 'repo-a', 100, 'auto', 'failed')`,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status) values ('ck-solo', 'repo-b', 70, 'manual', 'complete')`,
		`insert into commit_links (commit_hash, repository_id, checkpoint_id, linked_at) values ('c1', 'repo-a', 'ck-z-early', 51)`,
		`insert into commit_links (commit_hash, repository_id, checkpoint_id, linked_at) values ('c2', 'repo-a', 'ck-b-tie', 101)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func TestMigration000002_UpgradePath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")

	db := openRaw(t, dbPath)
	if err := migrateWithSource(db, schemaAt(t, 1), 1); err != nil {
		t.Fatalf("migrate to 000001: %v", err)
	}
	seedV1(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("production migration: %v", err)
	}

	h, err := Open(ctx, dbPath, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer func() { _ = Close(h) }()

	rows, err := h.Queries.ListCheckpointsByRepository(ctx, sqldb.ListCheckpointsByRepositoryParams{
		RepositoryID: "repo-a", Limit: 10,
	})
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("repo-a checkpoints = %d, want 3", len(rows))
	}

	wantSeq := map[string]int64{
		"ck-z-early": 1, // created_at 50
		"ck-a-tie":   2, // created_at 100, id tiebreak
		"ck-b-tie":   3,
		"ck-solo":    1, // repo-b sequences independently
	}
	for id, want := range wantSeq {
		var got int64
		if err := h.DB.QueryRowContext(ctx,
			"select repository_sequence from checkpoints where checkpoint_id = ?", id,
		).Scan(&got); err != nil {
			t.Fatalf("read sequence for %s: %v", id, err)
		}
		if got != want {
			t.Errorf("sequence(%s) = %d, want %d", id, got, want)
		}
	}

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-new", RepositoryID: "repo-a", CreatedAt: 200,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	var got int64
	if err := h.DB.QueryRowContext(ctx,
		"select repository_sequence from checkpoints where checkpoint_id = 'ck-new'",
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("new insert sequence = %d, want 4", got)
	}

	if _, err := h.DB.ExecContext(ctx,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status, repository_sequence)
		 values ('ck-dup', 'repo-a', 300, 'auto', 'pending', 4)`,
	); err == nil {
		t.Error("duplicate (repository_id, repository_sequence) insert should fail")
	}

	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func TestMigration000002_ConcurrentInsertsAllocateUniqueSequences(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := Open(ctx, dbPath, UserFacingOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Close(h) }()
	if _, err := h.DB.ExecContext(ctx,
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-c', '/tmp/c', 1, 1)`,
	); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hi, err := Open(ctx, dbPath, UserFacingOpenOptions())
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = Close(hi) }()
			errs[i] = hi.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
				CheckpointID: fmt.Sprintf("ck-%02d", i), RepositoryID: "repo-c",
				CreatedAt: 100, Kind: "auto", Status: "pending",
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent insert %d: %v", i, err)
		}
	}

	var distinct, total int
	if err := h.DB.QueryRowContext(ctx,
		`select count(distinct repository_sequence), count(*) from checkpoints where repository_id = 'repo-c'`,
	).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != n || distinct != n {
		t.Errorf("sequences: %d distinct of %d rows, want %d/%d", distinct, total, n, n)
	}
}

func TestMigration000007_UpDownOnExistingDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	db := openRaw(t, dbPath)

	// Seed a version 6 row before adding the column.
	if err := migrateWithSource(db, schemaAt(t, 6), 6); err != nil {
		t.Fatal(err)
	}
	seeds := []string{
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-a', '/tmp/a', 1, 1)`,
		`insert into checkpoints (checkpoint_id, repository_id, created_at, kind, status) values ('ck-1', 'repo-a', 1, 'auto', 'complete')`,
		`insert into checkpoint_stats (checkpoint_id, session_count, files_changed, ai_percentage) values ('ck-1', 3, 7, 42)`,
	}
	for _, s := range seeds {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	// Existing rows receive no inferred version.
	if err := migrateWithSource(db, schemaAt(t, 7), 7); err != nil {
		t.Fatal(err)
	}
	var version sql.NullString
	if err := db.QueryRow("select attribution_version from checkpoint_stats where checkpoint_id = 'ck-1'").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version.Valid {
		t.Errorf("legacy row version = %q, want NULL", version.String)
	}

	// Downgrading removes only the new column.
	if err := migrateWithSource(db, schemaAt(t, 7), 6); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select attribution_version from checkpoint_stats").Scan(&version); err == nil {
		t.Error("attribution_version still selectable after down migration")
	}
	var sessions, files int64
	var pct float64
	if err := db.QueryRow("select session_count, files_changed, ai_percentage from checkpoint_stats where checkpoint_id = 'ck-1'").Scan(&sessions, &files, &pct); err != nil {
		t.Fatal(err)
	}
	if sessions != 3 || files != 7 || pct != 42 {
		t.Errorf("row after down = (%d, %d, %v), want (3, 7, 42)", sessions, files, pct)
	}

	// Reapplying the migration leaves the version unknown.
	if err := migrateWithSource(db, schemaAt(t, 7), 7); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select attribution_version from checkpoint_stats where checkpoint_id = 'ck-1'").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version.Valid {
		t.Errorf("re-upgraded row version = %q, want NULL", version.String)
	}
}

func TestMigration000008_UpDownOnExistingDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	db := openRaw(t, dbPath)

	// Migrate to 7 and seed a manifest row that predates the response columns.
	if err := migrateWithSource(db, schemaAt(t, 7), 7); err != nil {
		t.Fatal(err)
	}
	seeds := []string{
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-a', '/tmp/a', 1, 1)`,
		`insert into agent_sources (source_id, repository_id, provider, source_key, last_seen_at, created_at) values ('src-1', 'repo-a', 'claude_code', '/k', 1, 1)`,
		`insert into agent_sessions (session_id, provider_session_id, repository_id, provider, source_id, started_at, last_seen_at, metadata_json) values ('sess-1', 'p1', 'repo-a', 'claude_code', 'src-1', 1, 1, '{}')`,
		`insert into provenance_manifests (manifest_id, repository_id, session_id, turn_id, provider, kind, started_at, status, created_at, updated_at) values ('m-1', 'repo-a', 'sess-1', 't-1', 'claude_code', 'turn_bundle', 1, 'packaged', 1, 1)`,
	}
	for _, s := range seeds {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	// Up: the column exists and legacy rows are NULL.
	if err := migrateWithSource(db, schemaAt(t, 8), 8); err != nil {
		t.Fatal(err)
	}
	var status sql.NullString
	if err := db.QueryRow("select response_status from provenance_manifests where manifest_id = 'm-1'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status.Valid {
		t.Errorf("legacy row response_status = %q, want NULL", status.String)
	}

	// Down: the columns are removed, the rest of the row survives.
	if err := migrateWithSource(db, schemaAt(t, 8), 7); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select response_status from provenance_manifests").Scan(&status); err == nil {
		t.Error("response_status still selectable after down migration")
	}
	var pStatus string
	if err := db.QueryRow("select status from provenance_manifests where manifest_id = 'm-1'").Scan(&pStatus); err != nil {
		t.Fatal(err)
	}
	if pStatus != "packaged" {
		t.Errorf("row status after down = %q, want packaged", pStatus)
	}
}

func TestMigration000008_PartialMigrationNotClean(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	db := openRaw(t, dbPath)
	if err := migrateWithSource(db, schemaAt(t, 7), 7); err != nil {
		t.Fatal(err)
	}
	// Add every migration column except the final one.
	for _, col := range []string{"response_event_id", "response_hash", "response_summary", "response_status"} {
		if _, err := db.Exec("alter table provenance_manifests add column " + col + " text"); err != nil {
			t.Fatal(err)
		}
	}

	applied, err := dirtyProbes[8](ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("partial migration reported as applied; probe must require the final column")
	}

	// The final column marks the migration as applied.
	if _, err := db.Exec("alter table provenance_manifests add column response_completed_at integer"); err != nil {
		t.Fatal(err)
	}
	applied, err = dirtyProbes[8](ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Error("complete migration reported as not applied")
	}
}

func TestMigration000009_UpDownOnExistingDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	db := openRaw(t, dbPath)
	if err := migrateWithSource(db, schemaAt(t, 8), 8); err != nil {
		t.Fatal(err)
	}
	seeds := []string{
		`insert into repositories (repository_id, root_path, created_at, enabled_at) values ('repo-a', '/tmp/a', 1, 1)`,
		`insert into agent_sources (source_id, repository_id, provider, source_key, last_seen_at, created_at) values ('src-1', 'repo-a', 'cursor', '/k', 1, 1)`,
		`insert into agent_sessions (session_id, provider_session_id, repository_id, provider, source_id, started_at, last_seen_at, metadata_json) values ('sess-1', 'p1', 'repo-a', 'cursor', 'src-1', 1, 1, '{}')`,
		`insert into provenance_manifests (manifest_id, repository_id, session_id, turn_id, provider, kind, started_at, status, created_at, updated_at) values ('m-1', 'repo-a', 'sess-1', 't-1', 'cursor', 'turn_bundle', 1, 'packaged', 1, 1)`,
	}
	for _, statement := range seeds {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	if err := migrateWithSource(db, schemaAt(t, 9), 9); err != nil {
		t.Fatal(err)
	}
	var tokens sql.NullInt64
	if err := db.QueryRow("select tokens_in from provenance_manifests where manifest_id = 'm-1'").Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.Valid {
		t.Errorf("legacy tokens_in = %d, want NULL", tokens.Int64)
	}

	if err := migrateWithSource(db, schemaAt(t, 9), 8); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select tokens_in from provenance_manifests").Scan(&tokens); err == nil {
		t.Error("tokens_in still selectable after down migration")
	}
}

func TestMigration_DirtyAfterCommitClearsWithoutReplay(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	db := openRaw(t, dbPath)
	if _, err := db.Exec("update schema_migrations set dirty = 1"); err != nil {
		t.Fatal(err)
	}

	if err := migrateDB(ctx, db); err != nil {
		t.Fatalf("recovery after crash-after-commit: %v", err)
	}
	version, dirty := dbVersion(t, db)
	if version != latestMigrationVersion(t) || dirty {
		t.Fatalf("after recovery: version=%d dirty=%v, want latest clean", version, dirty)
	}
	if _, err := db.Exec("select repository_sequence from checkpoints limit 1"); err != nil {
		t.Errorf("applied schema must survive recovery: %v", err)
	}
}

func TestMigration_DirtyUnknownVersionFailsClosed(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	if err := MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	db := openRaw(t, dbPath)
	if _, err := db.Exec("update schema_migrations set version = 99, dirty = 1"); err != nil {
		t.Fatal(err)
	}

	err := migrateDB(ctx, db)
	if err == nil {
		t.Fatal("dirty version without a probe must fail closed")
	}
	if !strings.Contains(err.Error(), "no recovery probe") {
		t.Errorf("error should name the missing probe: %v", err)
	}
}

func TestMigration_EveryVersionHasDirtyProbe(t *testing.T) {
	entries, err := migrationsFS.ReadDir("schema")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err == nil {
			seen[v] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("no migrations found")
	}
	for v := range seen {
		if _, ok := dirtyProbes[v]; !ok {
			t.Errorf("migration %d has no dirty-recovery probe; add one to dirtyProbes", v)
		}
	}
}

func TestMigration000002_FailureRollsBackAndRecovers(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	db := openRaw(t, dbPath)

	if err := migrateWithSource(db, schemaAt(t, 1), 1); err != nil {
		t.Fatalf("migrate to 000001: %v", err)
	}
	seedV1(t, db)

	broken := schemaAt(t, 1)
	broken["000002_broken.up.sql"] = &fstest.MapFile{Data: []byte(
		"alter table checkpoints add column repository_sequence integer not null default 0;\n" +
			"insert into nonexistent_table values (1);\n",
	)}
	broken["000002_broken.down.sql"] = &fstest.MapFile{Data: []byte("select 1;\n")}

	if err := migrateWithSource(db, broken, 2); err == nil {
		t.Fatal("broken migration should fail")
	}

	if _, err := db.Exec("select repository_sequence from checkpoints limit 1"); err == nil {
		t.Error("rolled-back column should not exist")
	}
	var count int
	if err := db.QueryRow("select count(*) from checkpoints").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Errorf("seeded checkpoints = %d, want 4", count)
	}

	if err := migrateDB(ctx, db); err != nil {
		t.Fatalf("production migration after failure: %v", err)
	}
	version, dirty := dbVersion(t, db)
	if version != latestMigrationVersion(t) || dirty {
		t.Fatalf("after recovery: version=%d dirty=%v, want latest clean", version, dirty)
	}
	var seq int64
	if err := db.QueryRow(
		"select repository_sequence from checkpoints where checkpoint_id = 'ck-b-tie'",
	).Scan(&seq); err != nil {
		t.Fatalf("backfilled sequence unreadable after recovery: %v", err)
	}
	if seq != 3 {
		t.Errorf("backfilled sequence = %d, want 3", seq)
	}
}
