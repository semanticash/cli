package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/launcher"
	"github.com/semanticash/cli/internal/platform"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
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
	handoffPath := util.PreCommitCheckpointPath(semDir)

	// If Semantica isn't enabled, quietly no-op (hooks should never break commit).
	if !util.IsEnabled(semDir) {
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Print attribution summary
	printAttributionSummary(semDir)

	// Read checkpoint id produced by pre-commit.
	// If missing, do NOT fall back to "latest" (nondeterministic).
	handoffBytes, err := os.ReadFile(handoffPath)
	if err != nil {
		// No deterministic checkpoint to link (maybe commit ran with --no-verify, or pre-commit failed).
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}
	raw := strings.TrimSpace(string(handoffBytes))
	if raw == "" {
		_ = os.Remove(handoffPath)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	parts := strings.SplitN(raw, "|", 2)
	checkpointID := strings.TrimSpace(parts[0])

	if checkpointID == "" {
		_ = os.Remove(handoffPath)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Optional: prevent stale reuse (10 minute window)
	if len(parts) == 2 {
		if ts, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
			if time.Now().UnixMilli()-ts > 600_000 {
				_ = os.Remove(handoffPath)
				return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
			}
		}
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 50 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: open db failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Resolve repository row
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
		}
		util.AppendActivityLog(semDir, "post-commit warning: get repo row failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Get HEAD commit SHA
	sha, err := repo.HeadCommitHash(ctx)
	if err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: head commit hash failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, Linked: false}, nil
	}

	// Optional safety: verify checkpoint exists (pre-commit should have inserted it).
	if _, err := h.Queries.GetCheckpointByID(ctx, checkpointID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &PostCommitResult{
				RepoRoot:   repoRoot,
				CommitHash: sha,
				Linked:     false,
			}, nil
		}
		return nil, err
	}

	now := time.Now().UnixMilli()

	// Insert OR IGNORE, so idempotent.
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   sha,
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: checkpointID,
		LinkedAt:     now,
	}); err != nil {
		util.AppendActivityLog(semDir, "post-commit warning: insert commit link failed: %v", err)
		return &PostCommitResult{RepoRoot: repoRoot, CommitHash: sha, CheckpointID: checkpointID, Linked: false}, nil
	}

	// Spawn detached worker to complete the checkpoint (blobs, manifest, session reconciliation).
	spawnWorker(ctx, semDir, checkpointID, sha, repoRoot)

	// Best-effort delete handoff file so we never reuse it on a later commit.
	_ = os.Remove(handoffPath)

	return &PostCommitResult{
		RepoRoot:     repoRoot,
		CommitHash:   sha,
		CheckpointID: checkpointID,
		Linked:       true,
	}, nil
}

// printAttributionSummary reads the summary file written by the commit-msg hook
// and prints a one-line attribution summary to stderr. Deletes the file after reading.
func printAttributionSummary(semDir string) {
	path := util.CommitAttributionSummaryPath(semDir)
	data, err := os.ReadFile(path)
	_ = os.Remove(path) // always clean up

	if err != nil || len(data) == 0 {
		return
	}

	summary, ok := parseAttributionSummary(data)
	if !ok {
		return
	}

	fmt.Fprint(os.Stderr, summary.render())
}

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

// dispatchViaLauncher writes a pending marker and kickstarts the
// launchd worker. If kickstart fails, the marker stays on disk for a
// later drain.
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
	if err := launcher.Kickstart(ctx, launcher.DomainTarget()); err != nil {
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
