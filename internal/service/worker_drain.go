package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/launcher"
)

// DefaultDrainLinger is the bounded idle interval the drain loop
// observes after a pass finds no markers before committing to
// exit. The linger exists so a marker that lands on disk during
// the worker's final rescan (between discovery and process exit)
// still has a chance to be picked up by the next pass rather than
// stranded until the next commit-triggered kickstart. Two seconds
// is empirical: long enough to absorb realistic hook-to-kickstart
// timing, short enough that the worker exits promptly when there
// is genuinely nothing to do.
const DefaultDrainLinger = 2 * time.Second

// MarkerRunner is the signature the drain loop uses to execute a
// single marker. Extracting it behind a named function type lets
// the production path plug in WorkerService.Run while tests plug
// in a fake that records invocations, simulates errors, or writes
// a new marker mid-drain to exercise the repeat-until-stable
// loop. The type therefore doubles as a seam and a compact
// specification of the per-marker contract.
type MarkerRunner func(ctx context.Context, in WorkerInput) error

// DrainStats counts the per-marker outcomes from a single
// DrainOnce pass. It distinguishes "work done, marker gone"
// (Processed and Rejected) from "work partially done or not done
// at all, marker remains" (RunErrors and DeleteErrors) so
// DrainUntilStable can decide whether forward progress is still
// possible without conflating runner success with marker
// removal.
type DrainStats struct {
	// Processed is the number of markers whose run succeeded
	// and whose marker file was then successfully deleted. The
	// queue is strictly shorter after each one.
	Processed int

	// Rejected is the number of markers removed because their
	// contents could not be parsed or did not agree with their
	// queue location. The underlying work is lost, but the
	// queue is strictly shorter.
	Rejected int

	// RunErrors is the number of markers whose run returned a
	// non-nil error. The marker remains on disk for a later
	// DrainUntilStable invocation (triggered by a subsequent
	// commit kickstart) to retry.
	RunErrors int

	// DeleteErrors is the number of markers whose run succeeded
	// but whose delete failed. The work was performed, but the
	// marker file could not be removed; the marker remains on
	// disk. WorkerService.Run is idempotent at the checkpoint
	// level (a completed checkpoint short-circuits), so a later
	// retry of a delete-error marker performs near-zero work.
	DeleteErrors int
}

// Progress returns the number of markers that were removed from
// the queue this pass (Processed + Rejected). This is the
// forward-progress signal DrainUntilStable uses to decide
// whether to keep looping. Counting only "runner succeeded" would
// cause an infinite loop on delete failures; counting "marker
// discovered" would cause a premature exit when every run fails.
func (s DrainStats) Progress() int { return s.Processed + s.Rejected }

// DefaultMarkerRunner returns the production runner, which is
// simply WorkerService.Run. Kept behind a constructor so callers
// (and the cobra command wiring) do not need to know the struct
// name.
func DefaultMarkerRunner() MarkerRunner {
	return NewWorkerService().Run
}

// DrainOnce iterates every active repository in the broker
// registry and processes every pending marker it currently
// finds. For each marker, it reads and validates the marker in
// its queue context (see launcher.ReadInQueue), invokes run, and
// deletes the marker on success. Markers that fail to process
// are left in place so a later DrainUntilStable invocation can
// retry.
//
// Returns a DrainStats describing the per-outcome counts from
// this pass. A non-nil error indicates a broker-level failure
// that prevented discovery; per-marker failures are logged and
// reflected in the stats but do not abort the outer loop.
//
// DrainOnce is a single-pass operation with no memory across
// calls. Callers that need "retry a marker at most once per
// invocation" semantics should use DrainUntilStable, which
// layers a per-invocation skip set on top.
func DrainOnce(ctx context.Context, run MarkerRunner) (DrainStats, error) {
	return drainOncePass(ctx, run, nil)
}

// drainOutcome is the per-marker result of drainOne. Used as a
// small sum type rather than booleans so drainOncePass can both
// update stats and decide whether to add the marker to a skip
// set for future passes within the same DrainUntilStable call.
type drainOutcome int

const (
	// outcomeNone covers the benign race where a marker was
	// deleted between List and Read. Nothing happened, nothing
	// to count, nothing to skip.
	outcomeNone drainOutcome = iota

	// outcomeProcessed means the runner succeeded and the
	// marker file was deleted. Queue is shorter.
	outcomeProcessed

	// outcomeRejected means the marker could not be parsed or
	// validated, and its file was deleted to prevent infinite
	// retry on an unreadable entry. Queue is shorter.
	outcomeRejected

	// outcomeRunError means the runner returned a non-nil
	// error. The marker file remains on disk and the path is
	// added to the in-invocation skip set so subsequent passes
	// within the same DrainUntilStable call do not retry it.
	// The next DrainUntilStable invocation, triggered by a
	// later kickstart, starts with a fresh skip set and will
	// retry.
	outcomeRunError

	// outcomeDeleteError means the runner succeeded but the
	// marker file could not be removed. Same skip-set and
	// retry-deferral semantics as outcomeRunError; the work
	// has already been performed so a retry inside the same
	// invocation would be wasted even if it did short-circuit
	// via checkpoint idempotency.
	outcomeDeleteError
)

// drainOncePass is the private pass-level implementation shared
// by DrainOnce (skip == nil) and DrainUntilStable (skip is a
// per-invocation set that grows as markers fail). A path present
// in skip is not read, not run, and contributes no stat change.
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
			switch drainOne(ctx, run, root, p) {
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

// drainOne processes a single marker and returns the outcome.
// Every failure class is logged at the wlog level; the function
// never returns an error because per-marker failures are not a
// reason to abort the outer loop.
func drainOne(ctx context.Context, run MarkerRunner, root, path string) drainOutcome {
	m, err := launcher.ReadInQueue(root, path)
	if err != nil {
		// Benign race: another pass deleted the marker between
		// our List and Read. Skip silently.
		if errors.Is(err, os.ErrNotExist) {
			return outcomeNone
		}
		// Any other read failure means the marker is
		// unrecoverable: malformed JSON, missing required
		// fields, or a contextual mismatch with its queue
		// location (for example, a marker whose declared
		// RepoRoot does not match the queue it was found in).
		// Deleting the marker avoids an infinite retry loop on
		// a file we cannot act on anyway.
		wlog("worker: drain: reject corrupt marker %s: %v\n", path, err)
		if delErr := launcher.Delete(path); delErr != nil {
			wlog("worker: drain: delete corrupt marker %s: %v\n", path, delErr)
			return outcomeDeleteError
		}
		return outcomeRejected
	}

	if err := run(ctx, WorkerInput{
		CheckpointID: m.CheckpointID,
		CommitHash:   m.CommitHash,
		RepoRoot:     m.RepoRoot,
	}); err != nil {
		wlog("worker: drain: run checkpoint %s: %v\n", m.CheckpointID, err)
		return outcomeRunError
	}

	if err := launcher.Delete(path); err != nil {
		wlog("worker: drain: delete marker %s: %v\n", path, err)
		return outcomeDeleteError
	}
	return outcomeProcessed
}

// DrainUntilStable runs pass after pass until two consecutive
// passes make zero forward progress, with a bounded idle linger
// between them. Progress is measured as markers removed from the
// queue (DrainStats.Progress); a pass where every marker failed
// to run, or where every run succeeded but delete failed,
// contributes no Progress and causes the loop to enter the
// linger and then exit.
//
// A marker whose run or delete fails is added to a per-invocation
// skip set so subsequent passes within the same call do not read,
// retry, or re-run it. This means each failing marker is attempted
// at most once per DrainUntilStable invocation, even when other
// markers in the same pass succeed and keep the loop going. The
// next invocation, triggered by a later kickstart, starts with a
// fresh skip set and gets the retry.
//
// Consequences of this contract:
//
//   - A marker whose run keeps failing is left on disk for the
//     next kickstart-triggered invocation (typically the next
//     commit) to retry, rather than being retried in a tight
//     spin here.
//   - A marker whose run succeeded but whose deletion failed is
//     not retried inside this invocation either. The work was
//     performed; the file just cannot be removed. The next
//     invocation retries; WorkerService.Run is idempotent at
//     the checkpoint level so the retry cost is negligible.
//   - A mixed pass (some successes, some failures) does not
//     trigger in-invocation retries of the failures, because
//     the failures are held in the skip set.
//   - The linger still exists so a marker that arrives during
//     the moments between the last discovery and this loop's
//     exit decision can be picked up by the final pass.
//
// Passing linger <= 0 disables the idle wait, which is what
// tests typically want.
//
// Any error returned by an underlying drain pass aborts the
// loop. Callers that want to keep running after a broker-level
// failure should call DrainOnce directly.
func DrainUntilStable(ctx context.Context, linger time.Duration, run MarkerRunner) error {
	skip := map[string]bool{}
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
		if final.Progress() == 0 {
			return nil
		}
		// The linger scan caught work; loop back for another
		// full round rather than exiting.
	}
}
