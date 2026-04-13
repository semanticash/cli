package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	"github.com/semanticash/cli/internal/util"
)

// reconcileImplementations processes pending implementation observations.
// Best-effort: errors are logged, not propagated.
// Creates implementations.db on first call.
// Commit attachment is handled separately by handleImplementationPostCommit
// after session_checkpoints have been written.
func reconcileImplementations(ctx context.Context, repoRoot string) {
	base, err := broker.GlobalBase()
	if err != nil {
		return
	}
	implPath := filepath.Join(base, "implementations.db")

	implH, err := impldb.Open(ctx, implPath, impldb.OpenOptions{
		BusyTimeout: 5 * time.Second,
		TxImmediate: true,
	})
	if err != nil {
		wlog("worker: open implementations db: %v\n", err)
		return
	}
	defer func() { _ = impldb.Close(implH) }()

	r := &implementations.Reconciler{}
	if _, err := r.Reconcile(ctx, implH); err != nil {
		wlog("worker: reconcile implementations: %v\n", err)
	}
}

// handleImplementationPostCommit links a commit to its implementation.
// Must run after session_checkpoints have been written, because AttachCommit
// depends on commit_links + session_checkpoints to find which sessions
// belong to the commit's checkpoint.
func handleImplementationPostCommit(ctx context.Context, semDir, repoRoot, commitHash string) {
	base, err := broker.GlobalBase()
	if err != nil {
		wlog("worker: resolve implementations base: %v\n", err)
		return
	}
	implPath := filepath.Join(base, "implementations.db")

	implH, err := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	if err != nil {
		wlog("worker: open implementations db for commit attach: %v\n", err)
		return
	}
	defer func() { _ = impldb.Close(implH) }()

	r := &implementations.Reconciler{}
	if err := r.AttachCommit(ctx, implH, implementations.AttachCommitInput{
		RepoPath:   repoRoot,
		CommitHash: commitHash,
	}); err != nil {
		wlog("worker: attach commit to implementation: %v\n", err)
		return
	}

	if !util.IsImplementationSummaryEnabled(semDir) {
		return
	}

	canonicalPath := broker.CanonicalRepoPath(repoRoot)
	implID, err := implH.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: canonicalPath,
		CommitHash:    commitHash,
	})
	if err != nil {
		return
	}

	if ok, reason := implementations.ShouldAutoSummarize(ctx, implH, implID, implementations.ShouldAutoSummarizeOpts{}); !ok {
		wlog("worker: auto-impl-summary: skip %s: %s\n", implID[:8], reason)
		return
	}

	if err := implementations.MarkGenerationInProgress(ctx, implH, implID); err != nil {
		wlog("worker: auto-impl-summary: mark in-progress: %v\n", err)
		return
	}

	if !spawnAutoImplementationSummary(semDir, implID) {
		implementations.ClearGenerationInProgress(ctx, implH, implID)
	}
}

// spawnAutoImplementationSummary launches `semantica _auto-implementation-summary`
// as a detached process. Returns true on success, false on failure.
func spawnAutoImplementationSummary(semDir, implID string) bool {
	exe, err := os.Executable()
	if err != nil {
		exe = "semantica"
	}

	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		wlog("worker: auto-impl-summary: open log failed: %v\n", err)
		return false
	}

	cmd := exec.Command(exe, "_auto-implementation-summary",
		"--impl", implID,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	platform.DetachProcess(cmd)

	if err := cmd.Start(); err != nil {
		wlog("worker: auto-impl-summary: spawn failed: %v\n", err)
		_ = logFile.Close()
		return false
	}

	_ = logFile.Close()
	wlog("worker: auto-impl-summary spawned for %s\n", implID[:8])
	return true
}
