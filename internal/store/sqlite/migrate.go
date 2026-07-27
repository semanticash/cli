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
	"github.com/golang-migrate/migrate/v4/database"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "modernc.org/sqlite"
)

//go:embed schema/*.sql
var migrationsFS embed.FS

// dirtyProbes identifies whether each migration changed the schema.
// Every migration must register a probe.
var dirtyProbes = map[int]func(ctx context.Context, db *sql.DB) (bool, error){
	1: func(ctx context.Context, db *sql.DB) (bool, error) {
		return schemaHasTable(ctx, db, "checkpoints")
	},
	2: func(ctx context.Context, db *sql.DB) (bool, error) {
		return schemaHasColumn(ctx, db, "checkpoints", "repository_sequence")
	},
	3: func(ctx context.Context, db *sql.DB) (bool, error) {
		return schemaHasColumn(ctx, db, "checkpoints", "event_cursor")
	},
}

func schemaHasTable(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"select count(*) from sqlite_master where type = 'table' and name = ?", table,
	).Scan(&n)
	return n > 0, err
}

func schemaHasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"select count(*) from pragma_table_info(?) where name = ?", table, column,
	).Scan(&n)
	return n > 0, err
}

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

	// A dirty marker can represent either a committed or rolled-back
	// migration, so inspect the schema before changing its version.
	if version, dirty, verr := driver.Version(); verr == nil && dirty {
		probe, ok := dirtyProbes[version]
		if !ok {
			return fmt.Errorf("database is dirty at migration %d and no recovery probe exists for it; "+
				"inspect the schema before forcing a version", version)
		}
		applied, err := probe(ctx, db)
		if err != nil {
			return fmt.Errorf("probe dirty migration %d: %w", version, err)
		}
		if applied {
			// Crash after commit: keep the version, clear the flag.
			if err := driver.SetVersion(version, false); err != nil {
				return fmt.Errorf("clear dirty migration state: %w", err)
			}
		} else {
			// Rolled back: step back so Up() retries the migration.
			prev := version - 1
			if prev < 1 {
				prev = database.NilVersion
			}
			if err := driver.SetVersion(prev, false); err != nil {
				return fmt.Errorf("reset dirty migration state: %w", err)
			}
		}
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
