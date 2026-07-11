package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type TidyService struct{}

func NewTidyService() *TidyService { return &TidyService{} }

type TidyInput struct {
	RepoPath string
	Apply    bool
}

// TidyAction describes one reported cleanup item.
type TidyAction struct {
	Category string `json:"category"`
	ID       string `json:"id"`
	Detail   string `json:"detail"`
}

type TidyResult struct {
	DryRun               bool         `json:"dry_run"`
	BrokerEntriesPruned  int          `json:"broker_entries_pruned"`
	CaptureStatesRemoved int          `json:"capture_states_removed"`
	CheckpointsMarked    int          `json:"checkpoints_marked_failed"`
	Errors               int          `json:"errors,omitempty"`
	Actions              []TidyAction `json:"actions,omitempty"`
}

const (
	// Capture states older than this with a confirmed missing transcript are stale.
	captureStaleThreshold = 24 * time.Hour
	// Pending checkpoints older than this with no manifest/commit are abandoned.
	pendingStaleThreshold = 1 * time.Hour
)

func (s *TidyService) Tidy(ctx context.Context, in TidyInput) (*TidyResult, error) {
	result := &TidyResult{DryRun: !in.Apply}

	s.tidyBroker(ctx, in.Apply, result)
	s.tidyCaptureStates(ctx, in.Apply, result)
	if err := s.tidyRepo(ctx, in, result); err != nil {
		return result, err
	}

	return result, nil
}

// tidyBroker removes broker registry entries whose .semantica dir is
// confirmed missing (ErrNotExist only, not permission or I/O errors).
func (s *TidyService) tidyBroker(ctx context.Context, apply bool, result *TidyResult) {
	regPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(regPath); err != nil {
		return
	}

	bh, err := broker.Open(ctx, regPath)
	if err != nil {
		return
	}

	// Identify stale entries for reporting.
	all, _ := broker.ListAllRepos(ctx, bh)
	var staleActions []TidyAction
	for _, r := range all {
		semDir := filepath.Join(r.Path, ".semantica")
		if isConfirmedMissing(semDir) {
			staleActions = append(staleActions, TidyAction{
				Category: "broker",
				ID:       r.Path,
				Detail:   ".semantica directory missing",
			})
		}
	}

	if len(staleActions) == 0 {
		return
	}

	if apply {
		removed, err := broker.PruneConfirmedMissing(ctx, bh)
		if err != nil {
			result.Errors++
			return
		}
		result.BrokerEntriesPruned = removed
		result.Actions = append(result.Actions, staleActions[:removed]...)
	} else {
		result.BrokerEntriesPruned = len(staleActions)
		result.Actions = append(result.Actions, staleActions...)
	}
}

// tidyCaptureStates removes capture state files that are clearly abandoned.
func (s *TidyService) tidyCaptureStates(ctx context.Context, apply bool, result *TidyResult) {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(states) == 0 {
		return
	}

	threshold := time.Now().Add(-captureStaleThreshold).UnixMilli()

	for _, st := range states {
		if !isCaptureStale(st, threshold) {
			continue
		}

		action := TidyAction{
			Category: "capture",
			ID:       st.Key(),
			Detail:   fmt.Sprintf("stale since %s", time.UnixMilli(st.Timestamp).UTC().Format(time.RFC3339)),
		}

		if apply {
			if err := hooks.DeleteCaptureStateByKey(st.Key()); err != nil {
				result.Errors++
				continue
			}
		}

		result.Actions = append(result.Actions, action)
		result.CaptureStatesRemoved++
	}
}

// isCaptureStale returns true if a capture state is safe to remove.
// A state is stale only when it is older than the threshold and its
// transcript file is confirmed missing. Permission errors, I/O failures,
// and states with an empty TranscriptRef are kept.
func isCaptureStale(st *hooks.CaptureState, thresholdMs int64) bool {
	if st.Timestamp > thresholdMs {
		return false
	}
	if st.TranscriptRef == "" {
		return false
	}
	return isConfirmedMissing(st.TranscriptRef)
}

// tidyRepo handles per-repo cleanup: pending checkpoints and orphan FTS rows.
func (s *TidyService) tidyRepo(ctx context.Context, in TidyInput, result *TidyResult) error {
	repoPath := in.RepoPath
	if repoPath == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil // not a git repo - skip repo-level cleanup
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	if !util.IsEnabled(semDir) {
		return nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil // DB inaccessible - skip
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil
	}

	// Mark stale pending checkpoints as failed.
	threshold := time.Now().Add(-pendingStaleThreshold).UnixMilli()
	stale, err := h.Queries.ListStalePendingCheckpoints(ctx, sqldb.ListStalePendingCheckpointsParams{
		RepositoryID: repoRow.RepositoryID,
		BeforeTs:     threshold,
	})
	if err == nil {
		for _, cp := range stale {
			action := TidyAction{
				Category: "checkpoint",
				ID:       util.ShortID(cp.CheckpointID),
				Detail:   fmt.Sprintf("pending since %s, no manifest or commit link", time.UnixMilli(cp.CreatedAt).UTC().Format(time.RFC3339)),
			}

			if in.Apply {
				if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
					CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
					CheckpointID: cp.CheckpointID,
				}); err != nil {
					result.Errors++
					continue
				}
			}

			result.Actions = append(result.Actions, action)
			result.CheckpointsMarked++
		}
	}

	return nil
}

// isConfirmedMissing returns true only for os.ErrNotExist. Permission errors,
// I/O errors, and other transient failures return false (keep the entry).
func isConfirmedMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}
