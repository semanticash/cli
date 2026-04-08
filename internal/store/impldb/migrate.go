package impldb

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "modernc.org/sqlite"
)

//go:embed schema/*.sql
var migrationsFS embed.FS

func migratePath(ctx context.Context, dbPath string, opts OpenOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 250 * time.Millisecond
	}
	if opts.Synchronous == "" {
		opts.Synchronous = "NORMAL"
	}

	dsn := sqliteDSN(dbPath, opts)

	// Dedicated DB connection just for migrations
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open sqlite for migrate: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

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
	defer func() {
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}

	return nil
}
