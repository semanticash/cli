package impldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	_ "modernc.org/sqlite"
)

type OpenOptions struct {
	BusyTimeout time.Duration
	Synchronous string
	TxImmediate bool // if true, all BEGIN statements use IMMEDIATE locking
}

// DefaultOpenOptions returns the standard options used by most callers:
// BusyTimeout 250ms, Synchronous "NORMAL".
func DefaultOpenOptions() OpenOptions {
	return OpenOptions{
		BusyTimeout: 250 * time.Millisecond,
		Synchronous: "NORMAL",
	}
}

type Handle struct {
	DB      *sql.DB
	Queries *impldbgen.Queries
}

func Open(ctx context.Context, dbPath string, opts OpenOptions) (*Handle, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("dbPath is empty")
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}

	// Defaults
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 250 * time.Millisecond
	}
	if opts.Synchronous == "" {
		opts.Synchronous = "NORMAL"
	}

	// Apply pending schema migrations before opening.
	if err := migratePath(ctx, dbPath, opts); err != nil {
		return nil, fmt.Errorf("auto-migrate: %w", err)
	}

	dsn := sqliteDSN(dbPath, opts)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := applyPragmas(ctx, db, opts); err != nil {
		_ = db.Close()
		return nil, err
	}

	h := &Handle{
		DB:      db,
		Queries: impldbgen.New(db),
	}
	return h, nil
}

// OpenNoMigrate opens the database without running migrations.
// Use this on hot paths (e.g., broker observation emission) where the
// database is expected to already exist and be at the current schema version.
// Returns an error if the database file does not exist.
func OpenNoMigrate(ctx context.Context, dbPath string, opts OpenOptions) (*Handle, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("dbPath is empty")
	}

	// Check the file exists — don't create it here.
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("impldb not found: %w", err)
	}

	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 250 * time.Millisecond
	}
	if opts.Synchronous == "" {
		opts.Synchronous = "NORMAL"
	}

	dsn := sqliteDSN(dbPath, opts)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := applyPragmas(ctx, db, opts); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Handle{
		DB:      db,
		Queries: impldbgen.New(db),
	}, nil
}

func Close(h *Handle) error {
	if h == nil || h.DB == nil {
		return nil
	}
	return h.DB.Close()
}

func RollbackTx(tx *sql.Tx) {
	if tx == nil {
		return
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Warn("impldb: rollback failed", "err", err)
	}
}

func applyPragmas(ctx context.Context, db *sql.DB, opts OpenOptions) error {
	busyMS := int(opts.BusyTimeout / time.Millisecond)
	if busyMS < 0 {
		busyMS = 0
	}

	stmts := []string{
		"PRAGMA journal_mode=WAL;",
		fmt.Sprintf("PRAGMA busy_timeout=%d;", busyMS),
		"PRAGMA foreign_keys=ON;",
		fmt.Sprintf("PRAGMA synchronous=%s;", opts.Synchronous),
	}

	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("apply pragma (%q): %w", s, err)
		}
	}
	return nil
}
