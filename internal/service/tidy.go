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
	"github.com/semanticash/cli/internal/toolsnap"
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
	ToolWindowsRecovered int          `json:"tool_windows_recovered,omitempty"`
	ToolWindowsRemoved   int          `json:"tool_windows_removed,omitempty"`
	TombstonesRemoved    int          `json:"tombstones_removed,omitempty"`
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
	repoRoot, err := s.tidyRepo(ctx, in, result)
	if err != nil {
		return result, err
	}
	if repoRoot != "" {
		s.tidyToolWindows(ctx, in.Apply, repoRoot, result)
	}

	return result, nil
}

// tidyBroker removes confirmed-stale broker registry entries.
// Unknown repo-state checks are reported but not pruned.
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

	all, _ := broker.ListAllRepos(ctx, bh)
	stale := make(map[string]broker.RepoStateReason)
	var actions []TidyAction
	for _, r := range all {
		state := broker.CheckRepoState(ctx, r.Path)
		switch state.Verdict {
		case broker.RepoStateStale:
			stale[r.CanonicalPath] = state.Reason
			actions = append(actions, TidyAction{
				Category: "broker",
				ID:       r.Path,
				Detail:   string(state.Reason),
			})
		case broker.RepoStateUnknown:
			result.Errors++
		}
	}

	if len(actions) == 0 {
		return
	}

	if apply {
		removed, err := broker.PruneStale(ctx, bh, stale)
		if err != nil {
			result.Errors++
			return
		}
		result.BrokerEntriesPruned = removed
		result.Actions = append(result.Actions, actions[:removed]...)
	} else {
		result.BrokerEntriesPruned = len(actions)
		result.Actions = append(result.Actions, actions...)
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

// tidyRepo cleans stale pending checkpoints and returns the enabled repo root.
func (s *TidyService) tidyRepo(ctx context.Context, in TidyInput, result *TidyResult) (string, error) {
	repoPath := in.RepoPath
	if repoPath == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return "", nil // not a git repo - skip repo-level cleanup
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	if !util.IsEnabled(semDir) {
		return "", nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return repoRoot, nil // DB inaccessible - skip
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return repoRoot, nil
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
				if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
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

	return repoRoot, nil
}

// tidyToolWindows reports or repairs recoverable and abandoned tool windows.
func (s *TidyService) tidyToolWindows(ctx context.Context, apply bool, repoRoot string, result *TidyResult) {
	snap, err := toolsnap.InspectRegistry(filepath.Join(repoRoot, ".semantica"))
	if err != nil {
		result.Errors++
		return
	}
	if !snap.Exists {
		return
	}
	now := time.Now()

	if apply {
		report, err := hooks.SweepToolWindows(ctx, repoRoot)
		if err != nil {
			// Include recovery errors that preceded the terminal failure.
			result.Errors += report.Errors + 1
		} else {
			result.Errors += report.Errors
			result.ToolWindowsRecovered = report.PartialsReplayed + report.GroupsResumed + report.GroupsTerminal
			if result.ToolWindowsRecovered > 0 {
				result.Actions = append(result.Actions, TidyAction{
					Category: "toolwindow", ID: "recovery",
					Detail: fmt.Sprintf("replayed %d partials, resumed %d groups, %d terminal",
						report.PartialsReplayed, report.GroupsResumed, report.GroupsTerminal),
				})
			}
		}
		// Report reclamation even if later sweep work failed.
		if report.GroupsReclaimed > 0 {
			result.ToolWindowsRemoved += report.MembersTombstoned
			result.Actions = append(result.Actions, TidyAction{
				Category: "toolwindow", ID: "reclaim",
				Detail: fmt.Sprintf("reclaimed %d stuck group(s), %d member(s) tombstoned",
					report.GroupsReclaimed, report.MembersTombstoned),
			})
		}
	} else {
		result.ToolWindowsRecovered = len(snap.Partials) + len(snap.CompleteGroups())
		if result.ToolWindowsRecovered > 0 {
			result.Actions = append(result.Actions, TidyAction{
				Category: "toolwindow", ID: "recovery",
				Detail: fmt.Sprintf("%d partial records and %d groups recoverable",
					len(snap.Partials), len(snap.CompleteGroups())),
			})
		}
	}

	// Open the mutable registry only for apply.
	var reg *toolsnap.Registry
	if apply {
		reg, err = toolsnap.OpenRegistry(filepath.Join(repoRoot, ".semantica"))
		if err != nil {
			result.Errors++
			return
		}
	}

	// Dry runs report groups that an applied sweep would reclaim.
	if !apply {
		groupMembers := map[string][]toolsnap.PendingToolSnapshot{}
		for _, w := range snap.Windows {
			groupMembers[w.GroupID] = append(groupMembers[w.GroupID], w)
		}
		activeGroups := map[string]bool{}
		for _, w := range snap.Windows {
			if w.Status == "active" {
				activeGroups[w.GroupID] = true
			}
		}
		nowMs := now.UnixMilli()
		for gid, members := range groupMembers {
			meta := snap.Groups[gid]
			degraded := meta.Sealed || (nowMs >= meta.JoinUntil && activeGroups[gid])
			if !degraded {
				continue
			}
			result.ToolWindowsRemoved += len(members)
			result.Actions = append(result.Actions, TidyAction{
				Category: "toolwindow", ID: util.ShortID(gid),
				Detail: fmt.Sprintf("degraded group, %d member(s), reclaimable", len(members)),
			})
		}
	}

	// Retain tombstones long enough to reject delayed post hooks.
	tombstoneCutoff := now.Add(-toolsnap.DefaultStaleWindowAge).UnixMilli()
	result.Errors += len(snap.MalformedTombstones)
	expired := 0
	for _, tb := range snap.Tombstones {
		if tb.At >= tombstoneCutoff {
			continue
		}
		if apply {
			if err := reg.RemoveTombstone(tb.Key); err != nil {
				result.Errors++
				continue
			}
		}
		expired++
	}
	if expired > 0 {
		result.TombstonesRemoved = expired
		result.Actions = append(result.Actions, TidyAction{
			Category: "toolwindow", ID: "tombstones",
			Detail: fmt.Sprintf("%d aged tombstone(s)", expired),
		})
	}
}

// isConfirmedMissing returns true only for os.ErrNotExist. Permission errors,
// I/O errors, and other transient failures return false (keep the entry).
func isConfirmedMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}
