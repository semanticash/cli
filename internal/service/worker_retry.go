package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/launcher"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// Attempts are counted when claimed. Exhausted checkpoints require
// manual retry. Durations are variables so tests can shorten them.
var (
	retryInitialDelay = 30 * time.Second
	retryMaxDelay     = 30 * time.Minute
	leaseDuration     = 10 * time.Minute
)

const (
	retryMultiplier  = 2
	retryJitterFrac  = 0.2
	retryMaxAttempts = 5
)

// workerLeaseOwner identifies this process for lease claims.
var workerLeaseOwner = sync.OnceValue(func() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString()[:8])
})

// retryDelay computes the jittered backoff after the given attempt.
func retryDelay(attempt int64) time.Duration {
	d := retryInitialDelay
	for i := int64(1); i < attempt; i++ {
		d *= retryMultiplier
		if d >= retryMaxDelay {
			d = retryMaxDelay
			break
		}
	}
	jitter := 1 + retryJitterFrac*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}

// permanentError marks failures that retrying cannot resolve, such as
// invalid persisted data or an unsupported checkpoint kind.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// markPermanent classifies an error as deterministic for this
// checkpoint. Use only where retrying cannot change the outcome.
func markPermanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

func isPermanentError(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

// ErrRetryScheduled stops the queue until a checkpoint is due.
// Cause is set when the retry follows a fresh transient failure.
type ErrRetryScheduled struct {
	CheckpointID string
	At           time.Time
	Cause        error // the transient failure, when one just occurred
}

func (e *ErrRetryScheduled) Error() string {
	msg := fmt.Sprintf("checkpoint %s retry scheduled at %s", e.CheckpointID, e.At.Format(time.RFC3339))
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *ErrRetryScheduled) Unwrap() error { return e.Cause }

// ErrLeaseHeld stops the queue until its head checkpoint's lease expires.
type ErrLeaseHeld struct {
	CheckpointID string
	Until        time.Time
}

func (e *ErrLeaseHeld) Error() string {
	return fmt.Sprintf("checkpoint %s lease held until %s", e.CheckpointID, e.Until.Format(time.RFC3339))
}

// claimAndProcess atomically claims a checkpoint, runs enrichment, and
// records completion, a scheduled retry, or terminal failure.
func (s *WorkerService) claimAndProcess(ctx context.Context, repoRoot string, item queueItem) (bool, error) {
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return false, fmt.Errorf("open db for claim: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now().UnixMilli()
	owner := workerLeaseOwner()
	claimed, err := h.Queries.ClaimCheckpoint(ctx, sqldb.ClaimCheckpointParams{
		LeaseOwner:   sql.NullString{String: owner, Valid: true},
		LeaseUntil:   sql.NullInt64{Int64: now + leaseDuration.Milliseconds(), Valid: true},
		CheckpointID: item.CheckpointID,
		Now:          now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim checkpoint %s: %w", item.CheckpointID, err)
	}

	perr := workerProcess(s, ctx, WorkerInput{
		CheckpointID: item.CheckpointID,
		CommitHash:   item.CommitHash,
		RepoRoot:     repoRoot,
	})
	if perr == nil {
		return true, nil
	}

	if isPermanentError(perr) || claimed.AttemptCount >= retryMaxAttempts {
		reason := "permanent error"
		if !isPermanentError(perr) {
			reason = fmt.Sprintf("attempt %d/%d exhausted", claimed.AttemptCount, retryMaxAttempts)
		}
		wlog("worker: checkpoint %s failed terminally (%s): %v\n", item.CheckpointID, reason, perr)
		rows, ferr := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
			CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
			LastError:    sqlstore.NullStr(perr.Error()),
			CheckpointID: item.CheckpointID,
		})
		if ferr == nil && rows != 1 {
			ferr = fmt.Errorf("terminal transition affected %d rows", rows)
		}
		if ferr != nil {
			return true, errors.Join(perr, fmt.Errorf("terminal failure for %s was not recorded: %w", item.CheckpointID, ferr))
		}
		return true, perr
	}

	delay := retryDelay(claimed.AttemptCount)
	at := time.Now().Add(delay)
	wlog("worker: checkpoint %s attempt %d failed; retry in %s: %v\n",
		item.CheckpointID, claimed.AttemptCount, delay.Round(time.Second), perr)
	rows, rerr := h.Queries.ReleaseCheckpointForRetry(ctx, sqldb.ReleaseCheckpointForRetryParams{
		LastError:     sqlstore.NullStr(perr.Error()),
		NextAttemptAt: at.UnixMilli(),
		CheckpointID:  item.CheckpointID,
		LeaseOwner:    sql.NullString{String: owner, Valid: true},
	})
	if rerr == nil && rows != 1 {
		rerr = fmt.Errorf("retry transition affected %d rows (lease lost)", rows)
	}
	if rerr != nil {
		return true, errors.Join(perr, fmt.Errorf("retry schedule for %s was not recorded: %w", item.CheckpointID, rerr))
	}
	return true, &ErrRetryScheduled{CheckpointID: item.CheckpointID, At: at, Cause: perr}
}

// ResolveAndRetryCheckpoint resolves a checkpoint ID prefix within the
// repository and retries it via RetryCheckpoint.
func (s *WorkerService) ResolveAndRetryCheckpoint(ctx context.Context, repoRoot, idOrPrefix string) (string, error) {
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.UserFacingOpenOptions())
	if err != nil {
		return "", fmt.Errorf("open db: %w", err)
	}
	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		_ = sqlstore.Close(h)
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	matches, err := h.Queries.ResolveCheckpointByPrefix(ctx, sqldb.ResolveCheckpointByPrefixParams{
		CheckpointID: idOrPrefix + "%",
		RepositoryID: repo.RepositoryID,
	})
	_ = sqlstore.Close(h)
	if err != nil {
		return "", fmt.Errorf("resolve checkpoint: %w", err)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no checkpoint matches %q", idOrPrefix)
	case 1:
		return matches[0], s.RetryCheckpoint(ctx, repoRoot, matches[0])
	default:
		return "", fmt.Errorf("checkpoint prefix %q is ambiguous", idOrPrefix)
	}
}

// RetryCheckpoint resets a terminally failed checkpoint and drains its
// repository. The wake-up marker is written before the database reset
// so a pending checkpoint is never left without a future drain.
func (s *WorkerService) RetryCheckpoint(ctx context.Context, repoRoot, checkpointID string) error {
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.UserFacingOpenOptions())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	cp, err := h.Queries.GetCheckpointByID(ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("read checkpoint %s: %w", checkpointID, err)
	}
	if cp.Status != "failed" {
		return fmt.Errorf("checkpoint %s is not in a failed state (only terminally failed checkpoints can be retried)", checkpointID)
	}

	links, err := h.Queries.GetCommitLinksByCheckpoint(ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("resolve commit link: %w", err)
	}
	commitHash := ""
	if len(links) > 0 {
		commitHash = links[0].CommitHash
		if err := launcher.Write(launcher.Marker{
			CheckpointID: checkpointID,
			CommitHash:   commitHash,
			RepoRoot:     repoRoot,
			WrittenAt:    time.Now().UnixMilli(),
		}); err != nil {
			return fmt.Errorf("restore wake-up marker: %w", err)
		}
	}

	rows, err := h.Queries.RetryFailedCheckpoint(ctx, checkpointID)
	if err != nil {
		return fmt.Errorf("reset checkpoint %s: %w", checkpointID, err)
	}
	if rows == 0 {
		// The status changed underneath us; the marker (if written)
		// is cleaned up by the next drain.
		return fmt.Errorf("checkpoint %s is no longer in a failed state", checkpointID)
	}
	return s.Run(ctx, WorkerInput{CheckpointID: checkpointID, CommitHash: commitHash, RepoRoot: repoRoot})
}
