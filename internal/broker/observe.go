package broker

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// Observation is a lightweight record of broker-routed activity for a session.
// Written to the global implementations DB for later reconciliation.
type Observation struct {
	Provider           string
	ProviderSessionID  string
	ParentSessionID    string // empty if root
	SourceProjectPath  string // raw SourceProjectPath from RawEvent
	TargetRepoPath     string
	EventTs            int64 // unix ms
}

// implDBPath returns the path to the global implementations database.
func implDBPath() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "implementations.db"), nil
}

// EmitObservation writes a minimal observation to the global implementations DB.
// Fail-open: returns on any error without propagating.
// No git calls. No rule evaluation. Just an append-only insert.
func EmitObservation(ctx context.Context, obs Observation) {
	dbPath, err := implDBPath()
	if err != nil {
		slog.Debug("impldb: skip observation, no default path", "err", err)
		return
	}

	// Fast path: open without migration (most calls after the first).
	// Fallback: if the DB doesn't exist yet, create it with Open() (runs
	// migration once). This ensures the very first observation on a fresh
	// install is never lost.
	opts := impldb.OpenOptions{BusyTimeout: 50 * time.Millisecond}
	h, err := impldb.OpenNoMigrate(ctx, dbPath, opts)
	if err != nil {
		// Create the database on the first observation if it does not exist yet.
		h, err = impldb.Open(ctx, dbPath, opts)
		if err != nil {
			slog.Debug("impldb: skip observation, cannot open db", "err", err)
			return
		}
	}
	defer func() { _ = impldb.Close(h) }()

	if err := h.Queries.InsertObservation(ctx, impldbgen.InsertObservationParams{
		ObservationID:     uuid.NewString(),
		Provider:          obs.Provider,
		ProviderSessionID: obs.ProviderSessionID,
		ParentSessionID:   impldb.NullStr(obs.ParentSessionID),
		SourceProjectPath: impldb.NullStr(obs.SourceProjectPath),
		TargetRepoPath:    obs.TargetRepoPath,
		EventTs:           obs.EventTs,
		CreatedAt:         time.Now().UnixMilli(),
	}); err != nil {
		slog.Debug("impldb: skip observation, insert failed", "err", err)
	}
}
