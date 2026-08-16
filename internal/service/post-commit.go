package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/launcher"
	"github.com/semanticash/cli/internal/platform"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
)

type PostCommitService struct{}

func NewPostCommitService() *PostCommitService { return &PostCommitService{} }

type PostCommitResult struct {
	RepoRoot     string
	CommitHash   string
	CheckpointID string
	Linked       bool // false means "nothing to link" or already linked
}

func (s *PostCommitService) HandlePostCommit(ctx context.Context, repoPath string) (*PostCommitResult, error) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	// Disabled repositories are a no-op.
	if !util.IsEnabled(semDir) {
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Print the attribution summary prepared by commit-msg.
	printAttributionSummary(semDir)

	// Without a handoff, there is no checkpoint that can be linked safely.
	handoff, ok := readCommitHandoff(semDir)
	if !ok {
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	sha, err := repo.HeadCommitHash(ctx)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: head commit hash failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Verify that the handoff belongs to this commit. A mismatch is left intact
	// rather than linked to unrelated history.
	commitTree, err := repo.CommitTree(ctx, sha)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: resolve commit tree failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, Linked: false}, nil
	}
	if handoff.Tree != commitTree {
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, Linked: false}, nil
	}
	matches, err := commitMatchesHandoffParent(ctx, repo, sha, handoff.Head)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: resolve commit parent failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, Linked: false}, nil
	}
	if !matches {
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, Linked: false}, nil
	}

	// Older receipts must be linked first to preserve checkpoint order.
	backlogPending, err := commitReceiptsPending(semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: list receipts failed: %v", err)
		backlogPending = true
	}

	// Persist the committed receipt before opening SQLite.
	receipt, err := promoteToReceipt(semDir, sha, handoff)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: write receipt failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: handoff.CheckpointID, Linked: false}, nil
	}

	// Let the worker drain an existing backlog in order.
	if backlogPending {
		spawnWorkerFn(ctx, semDir, receipt.CheckpointID, sha, repoRoot)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: receipt.CheckpointID, Linked: false}, nil
	}

	// Fast path: link the receipt inline. Failures leave it for the worker.
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 50 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: open db failed (receipt kept): %v", err)
		spawnWorkerFn(ctx, semDir, receipt.CheckpointID, sha, repoRoot)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: receipt.CheckpointID, Linked: false}, nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID, err := sqlstore.EnsureRepository(ctx, h.Queries, repoRoot)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: ensure repo failed (receipt kept): %v", err)
		spawnWorkerFn(ctx, semDir, receipt.CheckpointID, sha, repoRoot)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: receipt.CheckpointID, Linked: false}, nil
	}

	if err := linkReceipt(ctx, h, repoID, receipt); err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: link commit failed (receipt kept): %v", err)
		spawnWorkerFn(ctx, semDir, receipt.CheckpointID, sha, repoRoot)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: receipt.CheckpointID, Linked: false}, nil
	}

	// Complete the checkpoint asynchronously.
	spawnWorkerFn(ctx, semDir, receipt.CheckpointID, sha, repoRoot)

	// The durable link supersedes the receipt.
	_ = removeCommitReceipt(semDir, sha)

	return &PostCommitResult{
		RepoRoot:     repoRoot,
		CommitHash:   sha,
		CheckpointID: receipt.CheckpointID,
		Linked:       true,
	}, nil
}

// printAttributionSummary prints and removes the commit-msg summary.
func printAttributionSummary(semDir string) {
	path := util.CommitAttributionSummaryPath(semDir)
	data, err := os.ReadFile(path)
	_ = os.Remove(path)

	if err != nil || len(data) == 0 {
		return
	}

	summary, ok := parseAttributionSummary(data)
	if !ok {
		return
	}

	fmt.Fprint(os.Stderr, summary.render())
}

// spawnWorkerFn dispatches the post-commit worker, a seam so tests can avoid
// launching a detached process.
var spawnWorkerFn = spawnWorker

// spawnWorker dispatches post-commit work through the launcher when
// enabled and otherwise falls back to the legacy detached spawn.
func spawnWorker(ctx context.Context, semDir, checkpointID, commitHash, repoRoot string) {
	if ctx.Err() != nil {
		return
	}

	switch err := dispatchViaLauncher(ctx, checkpointID, commitHash, repoRoot); {
	case err == nil:
		return
	case errors.Is(err, ErrLauncherNotEnabled):
	default:
		util.AppendActivityLog(
			semDir,
			"post-commit: launcher dispatch failed (%v); falling back to detached spawn",
			err,
		)
	}

	spawnDetached(ctx, semDir, checkpointID, commitHash, repoRoot)
}

// ErrLauncherNotEnabled reports that launcher dispatch is disabled.
var ErrLauncherNotEnabled = errors.New("launcher not enabled")

// dispatchViaLauncher writes a pending marker and asks the OS launcher
// to drain queued work. If dispatch fails, the marker stays on disk for
// a later drain or fallback worker run.
func dispatchViaLauncher(ctx context.Context, checkpointID, commitHash, repoRoot string) error {
	if !launcher.IsEnabled() {
		return ErrLauncherNotEnabled
	}

	marker := launcher.Marker{
		CheckpointID: checkpointID,
		CommitHash:   commitHash,
		RepoRoot:     repoRoot,
		WrittenAt:    time.Now().UnixMilli(),
	}
	if err := launcher.Write(marker); err != nil {
		return fmt.Errorf("write pending marker: %w", err)
	}

	// Self-heal before kicking: if the registered binary was replaced
	// since enable, re-register the service first. A failed refresh falls
	// through to the caller's detached-spawn fallback.
	if _, err := launcher.EnsureFreshBinary(ctx); err != nil {
		return err
	}

	if err := launcher.Kickstart(ctx, launcher.UnitTarget()); err != nil {
		return fmt.Errorf("kickstart: %w", err)
	}
	return nil
}

// spawnDetached launches `semantica worker run` as a detached
// background process.
func spawnDetached(ctx context.Context, semDir, checkpointID, commitHash, repoRoot string) {
	exe, err := os.Executable()
	if err != nil {
		exe = "semantica"
	}

	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: open worker log failed: %v", err)
		return
	}

	cmd := exec.Command(exe, "worker", "run",
		"--checkpoint", checkpointID,
		"--commit", commitHash,
		"--repo", repoRoot,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detached workers should not inherit short-lived loopback
	// proxies from the parent process. Keep real forward
	// proxies intact.
	cmd.Env = platform.WithoutLoopbackProxies(os.Environ())
	platform.DetachProcess(cmd)

	if err := cmd.Start(); err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: spawn worker failed: %v", err)
		_ = logFile.Close()
		return
	}

	// Close log fd in parent - child inherited it.
	_ = logFile.Close()
}
