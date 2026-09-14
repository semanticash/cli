package commands

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

func TestWorkerRunFinishesScheduledCaptureWithoutLauncher(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "code.txt")
	git("commit", "-m", "test")
	sha := git("rev-parse", "HEAD")
	semDir := filepath.Join(dir, ".semantica")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{RepositoryID: "repo", RootPath: dir, CreatedAt: now, EnabledAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{CheckpointID: "cp", RepositoryID: "repo", CreatedAt: now, Kind: "auto", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{CheckpointID: "cp", RepositoryID: "repo", CommitHash: sha, LinkedAt: now}); err != nil {
		t.Fatal(err)
	}
	key := toolsnap.ToolKey{RepositoryID: "repo", Provider: "codex", SessionID: "session", TurnID: "turn", ToolUseID: "missing-post"}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	group, err := reg.Begin(ctx, toolsnap.PendingToolSnapshot{Key: key, ToolName: "Bash", StartedAt: now - 1, TreeHash: "tree", HeadHash: sha, SnapshotRef: "refs/semantica/test", ObjectFormat: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	// Start from a durable retry left by another invocation. This exercises
	// errors without an in-memory captureNotSettled cause.
	deadline := time.Now().Add(200 * time.Millisecond).UnixMilli()
	record, err := json.Marshal(map[string]any{
		"version": 1, "checkpoint_id": "cp", "repository_id": "repo", "after": 0, "through": now,
		"deadline": deadline, "members": []map[string]any{{"key": key, "group_id": group}},
		"result": map[string]any{"status": "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.SaveCheckpointCapture(ctx, sqldb.SaveCheckpointCaptureParams{CheckpointID: "cp", RecordJson: string(record)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.ExecContext(ctx, "update checkpoints set next_attempt_at = ?, last_error = ? where checkpoint_id = ?", deadline, "capture_not_settled", "cp"); err != nil {
		t.Fatal(err)
	}
	cmd := NewWorkerRunCmd(&RootOptions{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--repo", dir, "--checkpoint", "cp", "--commit", sha})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("standalone worker exited instead of retrying: %v", err)
	}
	cp, err := h.Queries.GetCheckpointByID(ctx, "cp")
	if err != nil || cp.Status != "complete" || cp.AttemptCount != 1 || cp.LeaseOwner != (sql.NullString{}) {
		t.Fatalf("checkpoint did not settle: %+v, %v", cp, err)
	}
	result, err := service.NewAttributionService().AttributeCommit(ctx, service.AttributionInput{RepoPath: dir, CommitHash: sha})
	if err != nil || result.Capture == nil || result.Capture.Status != "incomplete" || result.HumanLines != 0 || result.UnattributedLines != 1 {
		t.Fatalf("lost capture gap after retry: %+v, %v", result, err)
	}
}
