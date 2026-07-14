package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// RepoStateReason explains why a broker entry cannot receive events.
// Each value is a confirmed stale state that tidy may prune.
type RepoStateReason string

const (
	// RepoStaleSemDirMissing: <repo>/.semantica does not exist.
	RepoStaleSemDirMissing RepoStateReason = "sem-dir-missing"
	// RepoStaleLineageDBMissing: <repo>/.semantica exists but lineage.db does not.
	RepoStaleLineageDBMissing RepoStateReason = "lineage-db-missing"
	// RepoStaleNoRepoRow: lineage.db opens but has no repositories row
	// for this path.
	RepoStaleNoRepoRow RepoStateReason = "no-repo-row"
	// RepoStaleSettingsDisabled: <repo>/.semantica/enabled marker is absent.
	RepoStaleSettingsDisabled RepoStateReason = "settings-disabled"
)

// RepoStateVerdict summarizes whether a broker entry is usable.
type RepoStateVerdict int

const (
	// RepoStateOK means the broker can safely route events to this repo.
	RepoStateOK RepoStateVerdict = iota
	// RepoStateStale means one of the four confirmed reasons applies;
	// pruning the registry entry is safe.
	RepoStateStale
	// RepoStateUnknown means an unexpected error (permission, IO, DB
	// corruption) blocked the check. Callers must not prune on Unknown.
	RepoStateUnknown
)

// RepoStateResult is the result of CheckRepoState.
// Reason is set for stale entries; Err is set for unknown entries.
type RepoStateResult struct {
	Verdict RepoStateVerdict
	Reason  RepoStateReason
	Err     error
}

// CheckRepoState reports whether the broker can write to a repo.
// Unknown errors are not prunable.
//
// The check is read-only: it may open lineage.db to look up the
// repositories row but never mutates it.
func CheckRepoState(ctx context.Context, repoPath string) RepoStateResult {
	semDir := filepath.Join(repoPath, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RepoStateResult{Verdict: RepoStateStale, Reason: RepoStaleSemDirMissing}
		}
		return RepoStateResult{Verdict: RepoStateUnknown, Err: fmt.Errorf("stat %s: %w", semDir, err)}
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RepoStateResult{Verdict: RepoStateStale, Reason: RepoStaleLineageDBMissing}
		}
		return RepoStateResult{Verdict: RepoStateUnknown, Err: fmt.Errorf("stat %s: %w", dbPath, err)}
	}

	// Stat the marker directly so permission or IO errors do not look
	// like settings-disabled.
	markerPath := filepath.Join(semDir, "enabled")
	if _, err := os.Stat(markerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RepoStateResult{Verdict: RepoStateStale, Reason: RepoStaleSettingsDisabled}
		}
		return RepoStateResult{Verdict: RepoStateUnknown, Err: fmt.Errorf("stat %s: %w", markerPath, err)}
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return RepoStateResult{Verdict: RepoStateUnknown, Err: fmt.Errorf("open lineage db %s: %w", dbPath, err)}
	}
	defer func() { _ = sqlstore.Close(h) }()

	if _, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RepoStateResult{Verdict: RepoStateStale, Reason: RepoStaleNoRepoRow}
		}
		return RepoStateResult{Verdict: RepoStateUnknown, Err: fmt.Errorf("get repo %s: %w", repoPath, err)}
	}

	return RepoStateResult{Verdict: RepoStateOK}
}

// ErrRepoStale means the target repo state is stale.
// Callers may drop these writes without reporting a hook failure.
type ErrRepoStale struct {
	RepoPath string
	Reason   RepoStateReason
}

func (e *ErrRepoStale) Error() string {
	return fmt.Sprintf("broker: repo %s stale (%s)", e.RepoPath, e.Reason)
}
