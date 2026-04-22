//go:build !windows

package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/launcher"
)

// Delete failures should not keep DrainUntilStable alive forever.
func TestDrainUntilStable_DeleteFailuresDoNotInfiniteLoop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX mode-bit enforcement, cannot simulate delete failure")
	}

	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "a")
	writeMarker(t, repo, "b")

	pendingDir := launcher.PendingDir(repo)
	// Remove write permission so os.Remove fails with EACCES.
	if err := os.Chmod(pendingDir, 0o500); err != nil {
		t.Fatalf("chmod pending dir: %v", err)
	}
	t.Cleanup(func() {
		// Restore write permission so temp-dir cleanup succeeds.
		_ = os.Chmod(pendingDir, 0o755)
	})

	var runner recordingRunner
	done := make(chan error, 1)
	go func() {
		done <- DrainUntilStable(context.Background(), 0, runner.Run)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DrainUntilStable: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DrainUntilStable did not exit; delete failure is driving an infinite loop")
	}

	// The markers should still be on disk because delete failed.
	if err := os.Chmod(pendingDir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	remaining, _ := launcher.List(repo)
	if len(remaining) != 2 {
		t.Errorf("both markers should still be on disk after delete failures, got %v", remaining)
	}

	// Each marker should be tried once per invocation.
	if got := len(runner.calls()); got != 2 {
		t.Errorf("runner invoked %d times; expected exactly 2 (one per marker)", got)
	}
}

// This Unix-only test forces repo-log open failure with directory mode bits.
func TestDrainOne_RepoLogOpenFailureFallsBackToLauncherWriter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX mode-bit enforcement; cannot simulate open failure")
	}
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "ckpt-fallback")

	// Make the repo log unavailable after the marker is written.
	semPath := filepath.Join(repo, ".semantica")
	if err := os.Chmod(semPath, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(semPath, 0o755) })

	var launcherBuf bytes.Buffer
	prev := wlogWriter
	wlogWriter = &launcherBuf
	defer func() { wlogWriter = prev }()

	runner := recordingRunner{
		OnCall: func(in WorkerInput) error {
			wlog("job-wlog-fallback: %s\n", in.CheckpointID)
			return nil
		},
	}
	if _, err := DrainOnce(context.Background(), runner.Run); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	// Per-job output should fall back to the launcher writer.
	if !bytes.Contains(launcherBuf.Bytes(), []byte("job-wlog-fallback: ckpt-fallback")) {
		t.Errorf("expected per-job wlog in launcher writer as fallback, got:\n%s", launcherBuf.String())
	}

	// The open failure itself should also be visible there.
	if !bytes.Contains(launcherBuf.Bytes(), []byte("open repo log for")) {
		t.Errorf("expected open-failure line in launcher writer, got:\n%s", launcherBuf.String())
	}
}
