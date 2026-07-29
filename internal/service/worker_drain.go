package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/launcher"
	"github.com/semanticash/cli/internal/util"
)

// DefaultDrainLinger is the idle wait before the drain loop exits.
const DefaultDrainLinger = 2 * time.Second

// MarkerRunner executes one marker.
type MarkerRunner func(ctx context.Context, in WorkerInput) error

// DrainStats counts outcomes from one drain pass.
type DrainStats struct {
	// Processed counts markers that ran and were deleted.
	Processed int

	// Rejected counts markers dropped as unreadable or invalid.
	Rejected int

	// RunErrors counts markers left on disk after runner failure.
	RunErrors int

	// EarliestRetry is the soonest scheduled-retry time reported by a
	// runner, zero when none was reported.
	EarliestRetry time.Time

	// DeleteErrors counts markers whose work ran but could not be
	// removed.
	DeleteErrors int
}

// Progress returns the number of markers removed from the queue.
func (s DrainStats) Progress() int { return s.Processed + s.Rejected }

// DefaultMarkerRunner returns the production marker runner backed
// by the given hook registry. Production callers must pass
// providers.NewHookRegistry() (the worker cobra command does so).
// A nil registry produces a runner that skips reconciliation
// silently. This is useful only for tests that exercise the drain
// loop without provider wiring.
func DefaultMarkerRunner(registry *hooks.Registry) MarkerRunner {
	return NewWorkerService(registry).Run
}

// DrainOnce processes the current marker set once. Broker-level
// failures stop the pass; per-marker failures are logged and kept in
// the returned stats.
func DrainOnce(ctx context.Context, run MarkerRunner) (DrainStats, error) {
	return drainOncePass(ctx, run, nil)
}

// drainOutcome is the result of one marker attempt.
type drainOutcome int

const (
	// outcomeNone covers the race where another pass already removed
	// the marker.
	outcomeNone drainOutcome = iota

	// outcomeProcessed means the marker ran and was deleted.
	outcomeProcessed

	// outcomeRejected means the marker was invalid and was deleted.
	outcomeRejected

	// outcomeRunError means the runner failed and the marker stays on
	// disk.
	outcomeRunError

	// outcomeDeleteError means the marker ran but could not be
	// deleted.
	outcomeDeleteError
)

// drainOncePass is the shared implementation for DrainOnce and
// DrainUntilStable.
func drainOncePass(ctx context.Context, run MarkerRunner, skip map[string]bool) (DrainStats, error) {
	var stats DrainStats

	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return stats, fmt.Errorf("drain: resolve registry path: %w", err)
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return stats, fmt.Errorf("drain: open broker: %w", err)
	}
	defer func() { _ = broker.Close(bh) }()

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return stats, fmt.Errorf("drain: list active repos: %w", err)
	}

	for _, r := range repos {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		root := r.CanonicalPath
		paths, err := launcher.List(root)
		if err != nil {
			wlog("worker: drain: list %s: %v\n", root, err)
			continue
		}
		for _, p := range paths {
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			if skip != nil && skip[p] {
				continue
			}
			outcome, retryAt := drainOne(ctx, run, root, p)
			if !retryAt.IsZero() && (stats.EarliestRetry.IsZero() || retryAt.Before(stats.EarliestRetry)) {
				stats.EarliestRetry = retryAt
			}
			switch outcome {
			case outcomeProcessed:
				stats.Processed++
			case outcomeRejected:
				stats.Rejected++
			case outcomeRunError:
				stats.RunErrors++
				if skip != nil {
					skip[p] = true
				}
			case outcomeDeleteError:
				stats.DeleteErrors++
				if skip != nil {
					skip[p] = true
				}
			}
		}
	}
	return stats, nil
}

// drainOne processes one marker and returns the outcome, plus the
// scheduled-retry time when the runner reported one.
func drainOne(ctx context.Context, run MarkerRunner, root, path string) (outcome drainOutcome, retryAt time.Time) {
	m, err := launcher.ReadInQueue(root, path)
	if err != nil {
		// Another pass already deleted the marker.
		if errors.Is(err, os.ErrNotExist) {
			return outcomeNone, retryAt
		}
		// Drop unreadable or mismatched markers so they do not loop.
		wlog("worker: drain: reject corrupt marker %s: %v\n", path, err)
		if delErr := launcher.Delete(path); delErr != nil {
			wlog("worker: drain: delete corrupt marker %s: %v\n", path, delErr)
			return outcomeDeleteError, retryAt
		}
		return outcomeRejected, retryAt
	}

	// Route this job's output to the repo-local worker log. Drain-loop
	// output outside this scope stays on the current default writer.
	restore := redirectWlogToRepoLog(m.RepoRoot)
	defer restore()

	if err := run(ctx, WorkerInput{
		CheckpointID: m.CheckpointID,
		CommitHash:   m.CommitHash,
		RepoRoot:     m.RepoRoot,
	}); err != nil {
		wlog("worker: drain: run checkpoint %s: %v\n", m.CheckpointID, err)
		var sched *ErrRetryScheduled
		if errors.As(err, &sched) {
			retryAt = sched.At
		}
		// Lease expiry is the next safe reclaim time.
		var held *ErrLeaseHeld
		if errors.As(err, &held) {
			retryAt = held.Until
		}
		return outcomeRunError, retryAt
	}

	if err := launcher.Delete(path); err != nil {
		wlog("worker: drain: delete marker %s: %v\n", path, err)
		return outcomeDeleteError, retryAt
	}
	return outcomeProcessed, retryAt
}

// redirectWlogToRepoLog points wlog and the default slog logger at
// <repoRoot>/.semantica/worker.log for one job and returns a restore
// function. If the repo log cannot be opened, logging stays on the
// current writers and the job still runs. Callers must treat
// wlogWriter as single-goroutine state.
func redirectWlogToRepoLog(repoRoot string) func() {
	semDir := filepath.Join(repoRoot, ".semantica")
	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		wlog("worker: drain: open repo log for %s: %v; routing this job to the default log\n", repoRoot, err)
		return func() {}
	}
	prev := wlogWriter
	wlogWriter = logFile
	restoreSlog := setSlogDefaultTo(logFile)
	return func() {
		restoreSlog()
		wlogWriter = prev
		_ = logFile.Close()
	}
}

// writerIs lets package tests compare the current wlog destination
// without exporting wlogWriter.
func writerIs(w io.Writer) bool { return wlogWriter == w }

// maxRetryDrainWait bounds how long a drain waits in-process. Later
// retries wait for a launcher interval, commit, or manual drain.
var maxRetryDrainWait = 10 * time.Minute

// DrainUntilStable keeps draining until two passes in a row remove no
// markers. It waits for retries due within maxRetryDrainWait; other
// failed markers remain queued for a later invocation.
func DrainUntilStable(ctx context.Context, linger time.Duration, run MarkerRunner) error {
	skip := map[string]bool{}
	waitForRetry := func(at time.Time) (bool, error) {
		delay := time.Until(at)
		if at.IsZero() || delay > maxRetryDrainWait {
			return false, nil
		}
		if delay > 0 {
			wlog("worker: drain: waiting %s for scheduled retry\n", delay.Round(time.Second))
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(delay):
			}
		}
		// The retry is due: clear the skip set so its marker reruns.
		clear(skip)
		return true, nil
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stats, err := drainOncePass(ctx, run, skip)
		if err != nil {
			return err
		}
		if stats.Progress() > 0 {
			continue
		}
		if retried, err := waitForRetry(stats.EarliestRetry); err != nil {
			return err
		} else if retried {
			continue
		}
		if linger <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(linger):
		}
		final, err := drainOncePass(ctx, run, skip)
		if err != nil {
			return err
		}
		if retried, err := waitForRetry(final.EarliestRetry); err != nil {
			return err
		} else if retried {
			continue
		}
		if final.Progress() == 0 {
			return nil
		}
		// The linger scan found more work.
	}
}
