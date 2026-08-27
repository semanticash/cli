package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// workspaceFreezeDeadline bounds the optional pre-commit freeze path.
var workspaceFreezeDeadline = 2 * time.Second

// tryPairWorkspaceObservation records a workspace freeze and its commit pair.
// It returns true once the durable handoff owns recovery of the commit checkpoint.
func (s *PreCommitService) tryPairWorkspaceObservation(ctx context.Context, h *sqlstore.Handle, repoID, repoRoot, semDir string, base commitHandoff, commitCheckpointID string, createdAt int64) bool {
	dctx, cancel := context.WithTimeout(ctx, workspaceFreezeDeadline)
	defer cancel()

	cursor, err := maxEventCursor(dctx, h)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: read event cursor failed: %v", err)
		return false
	}
	freeze, release := tryWorkspaceFreeze(dctx, repoRoot, semDir, uuid.NewString(), cursor)
	if freeze == nil {
		return false
	}
	if err := validateFreezePairing(*freeze, createdAt); err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: freeze pairing invalid: %v", err)
		release(dctx)
		return false
	}
	// Persist the pairing before inserting either checkpoint.
	upgraded := base
	upgraded.Freeze = freeze
	if err := writeCommitHandoff(semDir, upgraded); err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: upgrade handoff failed: %v", err)
		release(dctx)
		return false
	}
	if err := insertPairedCheckpoints(dctx, h, repoID, commitCheckpointID, createdAt, *freeze); err != nil {
		// The durable handoff lets receipt processing reconstruct the pair.
		util.AppendActivityLog(semDir, "pre-commit warning: paired insert failed: %v", err)
	}
	return true
}

// maxEventCursor returns the current event insertion boundary.
func maxEventCursor(ctx context.Context, h *sqlstore.Handle) (int64, error) {
	var c int64
	err := h.DB.QueryRowContext(ctx, "select coalesce(max(insert_seq), 0) from agent_events").Scan(&c)
	return c, err
}

// tryWorkspaceFreeze captures the worktree and protects its tree with a ref.
// The returned cleanup uses the caller's context.
func tryWorkspaceFreeze(ctx context.Context, repoRoot, semDir, observationID string, eventCursor int64) (*freezeHandoff, func(context.Context)) {
	rc, err := toolsnap.ResolveRepoContext(ctx, repoRoot)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: freeze resolve context failed: %v", err)
		return nil, nil
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: freeze open store failed: %v", err)
		return nil, nil
	}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: freeze open registry failed: %v", err)
		return nil, nil
	}
	fz, err := reg.FreezeWorkspace(ctx, store, observationID)
	if err != nil {
		util.AppendActivityLog(semDir, "pre-commit warning: workspace freeze failed: %v", err)
		return nil, nil
	}
	release := func(rctx context.Context) {
		if derr := store.DeleteRef(rctx, fz.Ref, fz.TreeHash); derr != nil {
			util.AppendActivityLog(semDir, "pre-commit warning: release freeze ref failed: %v", derr)
		}
	}
	return &freezeHandoff{ObservationID: observationID, Tree: fz.TreeHash, Ref: fz.Ref, EventCursor: eventCursor}, release
}

// validateFreezePairing validates the persisted workspace-freeze identity.
func validateFreezePairing(f freezeHandoff, createdAt int64) error {
	if f.ObservationID == "" {
		return fmt.Errorf("empty observation id")
	}
	if !isHexObjectID(f.Tree) {
		return fmt.Errorf("frozen tree %q is not a full object id", f.Tree)
	}
	if f.Ref != toolsnap.WorkspaceFreezeRef(f.ObservationID) {
		return fmt.Errorf("ref %q does not match observation %q", f.Ref, f.ObservationID)
	}
	if f.EventCursor < 0 {
		return fmt.Errorf("negative event cursor %d", f.EventCursor)
	}
	if createdAt <= 0 {
		return fmt.Errorf("non-positive created_at %d", createdAt)
	}
	return nil
}

// isHexObjectID reports whether s is a full lowercase SHA-1 or SHA-256 object id.
func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// insertPairedCheckpoints atomically inserts the observation before the commit.
func insertPairedCheckpoints(ctx context.Context, h *sqlstore.Handle, repoID, commitCheckpointID string, createdAt int64, freeze freezeHandoff) error {
	if err := validateFreezePairing(freeze, createdAt); err != nil {
		return fmt.Errorf("invalid freeze pairing: %w", err)
	}
	if repoID == "" || commitCheckpointID == "" {
		return fmt.Errorf("paired insert: empty repository or commit checkpoint id")
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer sqlstore.RollbackTx(tx)
	q := h.Queries.WithTx(tx)

	if err := q.InsertWorkspaceObservationCheckpoint(ctx, sqldb.InsertWorkspaceObservationCheckpointParams{
		CheckpointID: freeze.ObservationID,
		RepositoryID: repoID,
		CreatedAt:    createdAt,
		EventCursor:  sql.NullInt64{Int64: freeze.EventCursor, Valid: true},
	}); err != nil {
		return err
	}
	if err := q.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: commitCheckpointID,
		RepositoryID: repoID,
		CreatedAt:    createdAt,
		Kind:         string(CheckpointAuto),
		Trigger:      sql.NullString{String: "commit", Valid: true},
		Message:      sql.NullString{String: "Auto checkpoint", Valid: true},
		ManifestHash: sql.NullString{}, // NULL - filled by worker
		SizeBytes:    sql.NullInt64{},  // NULL - filled by worker
		Status:       "pending",
		CompletedAt:  sql.NullInt64{}, // NULL - filled by worker
	}); err != nil {
		return err
	}
	return tx.Commit()
}
