package health

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// workerLockLongHold is a warning threshold, not staleness evidence.
const workerLockLongHold = 10 * time.Minute

// checkWorkerLock probes the lock because its file persists after release.
func checkWorkerLock(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return nil
	}
	semDir := filepath.Join(opts.RepoPath, ".semantica")
	lockPath := service.WorkerLockPath(semDir)

	if _, err := os.Lstat(lockPath); err != nil {
		if os.IsNotExist(err) {
			checks := []Check{{
				Category: "worker", ID: "lock", Status: StatusOK,
				Message: "worker lock: not held (never acquired in this repo)",
			}}
			return append(checks, workerQueueCheck(ctx, opts, false)...)
		}
		return []Check{{
			Category: "worker", ID: "lock", Status: StatusWarn,
			Message: fmt.Sprintf("worker lock: cannot stat %s: %v", lockPath, err),
		}}
	}

	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return []Check{{
			Category: "worker", ID: "lock", Status: StatusWarn,
			Message: fmt.Sprintf("worker lock: cannot open for probe: %v", err),
		}}
	}
	defer func() { _ = f.Close() }()

	acquired, err := platform.TryLockFile(f)
	if err != nil {
		return []Check{{
			Category: "worker", ID: "lock", Status: StatusWarn,
			Message: fmt.Sprintf("worker lock: probe failed: %v", err),
		}}
	}
	if acquired {
		_ = platform.UnlockFile(f)
		checks := []Check{{
			Category: "worker", ID: "lock", Status: StatusOK,
			Message: "worker lock: free (acquirable)",
		}}
		return append(checks, workerQueueCheck(ctx, opts, false)...)
	}

	status := StatusOK
	msg := "worker lock: held (no holder metadata)"
	remediation := ""
	if info, ierr := service.ReadRepoLockInfo(lockPath); ierr == nil && info.PID != 0 {
		hold := time.Since(time.UnixMilli(info.AcquiredAt)).Round(time.Second)
		msg = fmt.Sprintf("worker lock: held for %s by PID %d", hold, info.PID)
		if info.CheckpointID != "" {
			msg += ", checkpoint " + info.CheckpointID
		}
		switch {
		case !processAlive(info.PID):
			status = StatusWarn
			msg += " (recorded holder is not alive; metadata is stale)"
			remediation = "another process holds the lock without metadata; re-run doctor, and if this persists check for stray semantica worker processes"
		case hold > workerLockLongHold:
			status = StatusWarn
			msg += " (unusually long hold)"
			remediation = "a long enrichment can be legitimate; if this persists check the worker log in .semantica/worker.log"
		}
	}
	checks := []Check{{
		Category: "worker", ID: "lock", Status: status,
		Message: msg, Remediation: remediation,
	}}
	return append(checks, workerQueueCheck(ctx, opts, true)...)
}

// queueState summarizes the commit-linked drain queue for doctor.
type queueState struct {
	pending     int    // pending checkpoints, due or scheduled
	overdue     int    // pending, due now, no live lease
	blockedBy   string // terminally failed checkpoint gating the queue
	blockedErr  string // its preserved last error
	blockedTry  int64  // attempts consumed before terminal failure
	nextRetryAt int64  // earliest scheduled retry (unix ms), 0 if none
	retrying    int64  // attempt count of the scheduled-retry head
}

// workerQueueCheck reports waiting, scheduled, or blocked work.
func workerQueueCheck(ctx context.Context, opts Options, lockHeld bool) []Check {
	qs, err := pendingQueueState(ctx, opts.RepoPath)
	if err != nil {
		return nil
	}
	if qs.blockedBy != "" {
		msg := fmt.Sprintf("queue: %d checkpoint(s) waiting, blocked by terminally failed checkpoint %s (attempts %d)",
			qs.pending, qs.blockedBy, qs.blockedTry)
		if qs.blockedErr != "" {
			msg += ": " + qs.blockedErr
		}
		return []Check{{
			Category: "worker", ID: "queue", Status: StatusWarn,
			Message:     msg,
			Remediation: fmt.Sprintf("address the cause, then run `semantica worker retry %s`", qs.blockedBy),
		}}
	}
	if qs.pending == 0 {
		return []Check{{
			Category: "worker", ID: "queue", Status: StatusOK,
			Message: "queue: no checkpoints waiting",
		}}
	}
	if qs.nextRetryAt > 0 && qs.overdue == 0 {
		in := time.Until(time.UnixMilli(qs.nextRetryAt)).Round(time.Second)
		return []Check{{
			Category: "worker", ID: "queue", Status: StatusOK,
			Message: fmt.Sprintf("queue: %d checkpoint(s) waiting, retry scheduled in %s (attempt %d)",
				qs.pending, in, qs.retrying+1),
		}}
	}
	status := StatusOK
	remediation := ""
	if !lockHeld {
		status = StatusWarn
		remediation = "pending work with a free lock; `git commit` normally wakes the worker, or run `semantica worker drain`"
	}
	return []Check{{
		Category: "worker", ID: "queue", Status: status,
		Message:     fmt.Sprintf("queue: %d checkpoint(s) waiting", qs.pending),
		Remediation: remediation,
	}}
}

// pendingQueueState summarizes distinct commit-linked queue rows.
func pendingQueueState(ctx context.Context, repoRoot string) (queueState, error) {
	var qs queueState
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	h, err := openLineage(ctx, dbPath)
	if err != nil {
		return qs, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return qs, err
	}
	rows, err := h.Queries.ListPendingCommitLinkedCheckpoints(ctx, repo.RepositoryID)
	if err != nil {
		return qs, err
	}
	// Failed rows already passed by a completed successor do not block
	// (history from before ordering was enforced).
	var maxCompleteSeq int64
	if latest, err := h.Queries.GetMostRecentCommitLinkedCheckpoint(ctx, repo.RepositoryID); err == nil {
		maxCompleteSeq = latest.RepositorySequence
	}
	now := time.Now().UnixMilli()
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.CheckpointID] {
			continue
		}
		seen[r.CheckpointID] = true
		// The first active failure gates all later rows.
		if r.Status == "failed" && qs.blockedBy == "" && r.RepositorySequence > maxCompleteSeq {
			qs.blockedBy = r.CheckpointID
			qs.blockedErr = r.LastError.String
			qs.blockedTry = r.AttemptCount
		}
		if r.Status != "pending" {
			continue
		}
		qs.pending++
		switch {
		case r.LeaseUntil.Int64 > now:
			// Live lease: being processed right now.
		case r.NextAttemptAt > now:
			if qs.nextRetryAt == 0 || r.NextAttemptAt < qs.nextRetryAt {
				qs.nextRetryAt = r.NextAttemptAt
				qs.retrying = r.AttemptCount
			}
		default:
			qs.overdue++
		}
	}
	return qs, nil
}

// checkUnownedCaptureStates reports state outside active repositories.
func checkUnownedCaptureStates(ctx context.Context) []Check {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil {
		return nil
	}
	var repos []broker.RegisteredRepo
	if regPath, err := broker.DefaultRegistryPath(); err == nil {
		if bh, err := broker.Open(ctx, regPath); err == nil {
			repos, _ = broker.ListActiveRepos(ctx, bh)
			_ = broker.Close(bh)
		}
	}

	var unowned, deferred, orphaned []string
	if orphans, err := hooks.LoadOrphanedCaptureStates(); err == nil {
		for _, s := range orphans {
			orphaned = append(orphaned, s.Provider+"/"+s.SessionID)
		}
	}
	for _, s := range states {
		if s.ScopedDeferrals > 0 {
			deferred = append(deferred, fmt.Sprintf("%s/%s (%d deferral(s))", s.Provider, s.SessionID, s.ScopedDeferrals))
		}
		owned := false
		for _, r := range repos {
			if s.CWD != "" && broker.PathBelongsToRepo(s.CWD, r.Path) {
				owned = true
				break
			}
		}
		if !owned {
			unowned = append(unowned, s.Provider+"/"+s.SessionID)
		}
	}
	var checks []Check
	if len(unowned) > 0 {
		checks = append(checks, Check{
			Category: "worker", ID: "unowned_capture_states", Status: StatusWarn,
			Message: fmt.Sprintf("%d capture state(s) not owned by any active repository (e.g. %s)",
				len(unowned), strings.Join(sampleOf(unowned), ", ")),
			Remediation: "these sessions cannot be reconciled by a repository worker; a hook event for the session will capture them, or run `semantica tidy` to review",
		})
	}
	if len(deferred) > 0 {
		checks = append(checks, Check{
			Category: "worker", ID: "cross_repo_capture_states", Status: StatusWarn,
			Message: fmt.Sprintf("%d capture state(s) deferred by scoped reconciliation (e.g. %s)",
				len(deferred), strings.Join(sampleOf(deferred), ", ")),
			Remediation: "the session spans repositories; a later unscoped completion or incremental capture can replay the pending segment",
		})
	}
	if len(orphaned) > 0 {
		checks = append(checks, Check{
			Category: "worker", ID: "orphaned_capture_segments", Status: StatusWarn,
			Message: fmt.Sprintf("%d deferred transcript segment(s) could not be recovered (session moved to a new transcript; e.g. %s)",
				len(orphaned), strings.Join(sampleOf(orphaned), ", ")),
			Remediation: "evidence from these sessions may be incomplete; the snapshot keeps the transcript reference and offset for manual review",
		})
	}
	return checks
}

func sampleOf(items []string) []string {
	if len(items) > 3 {
		return items[:3]
	}
	return items
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}
