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
	Provider          string
	ProviderSessionID string
	ParentSessionID   string // empty if root
	SourceProjectPath string // raw SourceProjectPath from RawEvent
	TargetRepoPath    string
	EventTs           int64 // unix ms
}

// implDBPath returns the path to the global implementations database.
func implDBPath() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "implementations.db"), nil
}

// EmitObservations writes a batch of observations to the global implementations
// DB in a single open/transaction/close cycle. Fail-open: returns on any error
// without propagating. No git calls. No rule evaluation.
func EmitObservations(ctx context.Context, observations []Observation) {
	if len(observations) == 0 {
		return
	}

	dbPath, err := implDBPath()
	if err != nil {
		slog.Debug("impldb: skip observations, no default path", "err", err)
		return
	}

	// Fast path: open without migration.
	// Fallback: create the DB on first call.
	opts := impldb.OpenOptions{BusyTimeout: 50 * time.Millisecond}
	h, err := impldb.OpenNoMigrate(ctx, dbPath, opts)
	if err != nil {
		h, err = impldb.Open(ctx, dbPath, opts)
		if err != nil {
			slog.Debug("impldb: skip observations, cannot open db", "err", err)
			return
		}
	}
	defer func() { _ = impldb.Close(h) }()

	now := time.Now().UnixMilli()

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Debug("impldb: skip observations, begin tx failed", "err", err)
		return
	}
	qtx := h.Queries.WithTx(tx)

	for _, obs := range observations {
		if err := qtx.InsertObservation(ctx, impldbgen.InsertObservationParams{
			ObservationID:     uuid.NewString(),
			Provider:          obs.Provider,
			ProviderSessionID: obs.ProviderSessionID,
			ParentSessionID:   impldb.NullStr(obs.ParentSessionID),
			SourceProjectPath: impldb.NullStr(obs.SourceProjectPath),
			TargetRepoPath:    obs.TargetRepoPath,
			EventTs:           obs.EventTs,
			CreatedAt:         now,
		}); err != nil {
			slog.Debug("impldb: observation insert failed, rolling back batch", "err", err)
			_ = tx.Rollback()
			return
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Debug("impldb: observations commit failed", "err", err)
	}
}

// EmitObservation writes a single observation. Convenience wrapper around
// EmitObservations for callers outside the broker write path.
func EmitObservation(ctx context.Context, obs Observation) {
	EmitObservations(ctx, []Observation{obs})
}
