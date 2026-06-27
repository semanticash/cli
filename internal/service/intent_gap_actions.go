package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/intentgap"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// sqliteAgentActionLoader reads commit-linked agent actions from
// lineage.db and the local payload store. Payload fetches are bounded
// to events the extractor can use them on; events that need no
// payload skip the read.
type sqliteAgentActionLoader struct {
	repoRoot string
}

// newSQLiteAgentActionLoader binds a loader to one repository.
func newSQLiteAgentActionLoader(repoRoot string) intentgap.AgentActionLoader {
	return &sqliteAgentActionLoader{repoRoot: repoRoot}
}

func (l *sqliteAgentActionLoader) LoadActionsForCommits(ctx context.Context, commitHashes []string) ([]intentgap.BundleAgentAction, error) {
	if len(commitHashes) == 0 {
		return nil, nil
	}
	semDir := filepath.Join(l.repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	// A missing database is a valid empty state; an unreadable one is not.
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		return nil, nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 50 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil, intentgap.ErrLineageUnavailable
	}
	defer func() { _ = sqlstore.Close(h) }()

	// The blob store is best-effort. When it can't be opened, the
	// loader still surfaces tool_uses-derived actions and falls back
	// to the unknown-path action for Bash events.
	var bs *blobs.Store
	if s, bErr := blobs.NewStore(filepath.Join(semDir, "objects")); bErr == nil {
		bs = s
	}

	// One checkpoint can back several commits in the same range. The
	// per-commit query can then return the same event more than once.
	// Dedup by ActionID so each event maps to one bundle entry.
	var out []intentgap.BundleAgentAction
	seen := map[string]bool{}
	for _, commit := range commitHashes {
		rows, qErr := h.Queries.ListAgentActionsForCommit(ctx, commit)
		if qErr != nil {
			if errors.Is(qErr, context.Canceled) || errors.Is(qErr, context.DeadlineExceeded) {
				return out, qErr
			}
			// Treat query failure as lineage trouble, mirroring the
			// turn loader. Returning an empty result here would make
			// the action evidence incomplete.
			return nil, intentgap.ErrLineageUnavailable
		}
		for _, r := range rows {
			row := intentgap.ActionEventRow{
				EventID:      r.EventID,
				Provider:     r.Provider,
				TurnID:       nullStringValue(r.TurnID),
				CheckpointID: r.CheckpointID,
				TS:           r.Ts,
				ToolUses:     nullStringValue(r.ToolUses),
			}
			if row.TurnID == "" || row.ToolUses == "" {
				continue
			}
			var payload []byte
			if intentgap.NeedsPayload(row.ToolUses) && bs != nil && r.PayloadHash.Valid && r.PayloadHash.String != "" {
				if data, getErr := bs.Get(ctx, r.PayloadHash.String); getErr == nil {
					payload = data
				} else {
					slog.Debug("intent-gap action loader: payload fetch failed",
						"event_id", row.EventID, "err", getErr)
				}
			}
			for _, a := range intentgap.ExtractActions(row, payload, l.repoRoot) {
				if seen[a.ActionID] {
					continue
				}
				seen[a.ActionID] = true
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// nullStringValue returns the underlying string when valid, "" otherwise.
func nullStringValue(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}
