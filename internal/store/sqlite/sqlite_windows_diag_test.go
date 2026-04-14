package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	_ "modernc.org/sqlite"
)

// TestWindowsDiag_SQLiteBasicOpen isolates where SQLite fails on Windows.
// Each step is independent so the CI log shows exactly which layer breaks.
func TestWindowsDiag_SQLiteBasicOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "diag.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)", filepath.ToSlash(dbPath))

	t.Logf("DSN: %s", dsn)

	// Step 1: raw sql.Open + Ping
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("step 1: sql.Open failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		t.Fatalf("step 1: Ping failed: %v", err)
	}
	t.Log("step 1: sql.Open + Ping OK")

	// Step 2: CREATE TABLE
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS diag_test (id INTEGER PRIMARY KEY, val TEXT)"); err != nil {
		t.Fatalf("step 2: CREATE TABLE failed: %v", err)
	}
	t.Log("step 2: CREATE TABLE OK")

	// Step 3: INSERT + SELECT
	if _, err := db.Exec("INSERT INTO diag_test (val) VALUES (?)", "hello"); err != nil {
		t.Fatalf("step 3: INSERT failed: %v", err)
	}
	var val string
	if err := db.QueryRow("SELECT val FROM diag_test WHERE id = 1").Scan(&val); err != nil {
		t.Fatalf("step 3: SELECT failed: %v", err)
	}
	if val != "hello" {
		t.Fatalf("step 3: got %q, want hello", val)
	}
	t.Log("step 3: INSERT + SELECT OK")

	// Step 4: schema_migrations table (what golang-migrate creates)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (version uint64, dirty bool);
		CREATE UNIQUE INDEX IF NOT EXISTS version_unique ON schema_migrations (version);
	`); err != nil {
		t.Fatalf("step 4: schema_migrations table failed: %v", err)
	}
	t.Log("step 4: schema_migrations table OK")

	// Step 5: golang-migrate WithInstance on the warm handle
	driver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		t.Fatalf("step 5: migratesqlite.WithInstance failed: %v", err)
	}
	_ = driver
	t.Log("step 5: migratesqlite.WithInstance OK")

	// Step 6: migrateDB on the same warm handle (the production path)
	if err := migrateDB(t.Context(), db); err != nil {
		t.Fatalf("step 6: migrateDB (same handle) failed: %v", err)
	}
	t.Log("step 6: migrateDB OK")

	// Step 7: full Open flow (single handle: open, ping, pragma, migrate)
	_ = db.Close()
	dbPath2 := filepath.Join(t.TempDir(), "open_test.db")
	h, err := Open(t.Context(), dbPath2, DefaultOpenOptions())
	if err != nil {
		t.Fatalf("step 7: Open failed: %v", err)
	}
	_ = Close(h)
	t.Log("step 7: Open (full flow) OK")

	// Step 8: MigratePath standalone (opens its own warm handle)
	dbPath3 := filepath.Join(t.TempDir(), "migrate_standalone.db")
	if err := MigratePath(t.Context(), dbPath3); err != nil {
		t.Fatalf("step 8: MigratePath standalone failed: %v", err)
	}
	t.Log("step 8: MigratePath standalone OK")
}

// TestWindowsDiag_SimpleDSN tests with the simplest possible DSN
// (no pragmas, no file: prefix) to isolate URI parsing issues.
func TestWindowsDiag_SimpleDSN(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "simple.db")

	t.Logf("plain path: %s", dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open with plain path failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		t.Fatalf("Ping with plain path failed: %v", err)
	}
	t.Log("plain path: Ping OK")

	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("CREATE TABLE with plain path failed: %v", err)
	}
	t.Log("plain path: CREATE TABLE OK")
}
