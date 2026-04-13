package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "modernc.org/sqlite"
)

//go:embed schema/*.sql
var migrationsFS embed.FS

// MigratePath opens a temporary connection, runs migrations, and closes it.
// Used by tests that need a migrated DB without a full Handle.
func MigratePath(ctx context.Context, dbPath string) error {
	opts := DefaultOpenOptions()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("mkdir db dir: %w", err)
	}

	dsn := sqliteDSN(dbPath, opts)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open sqlite for migrate: %w", err)
	}
	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := applyPragmas(ctx, db, opts); err != nil {
		return err
	}

	return migrateDB(ctx, db)
}

// migrateDB runs embedded schema migrations on an already-open DB handle.
// Does not open or close its own connection.
func migrateDB(ctx context.Context, db *sql.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	driver, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("create sqlite migrate driver: %w", err)
	}

	src, err := iofs.New(migrationsFS, "schema")
	if err != nil {
		return fmt.Errorf("create iofs source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("create migrate instance: %w", err)
	}
	// Do not call m.Close() here. The migrate library's Close() closes the
	// underlying *sql.DB, but we do not own it -- the caller does. The iofs
	// source has no resources to release (it wraps an embed.FS).

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}

	return nil
}
