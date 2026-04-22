//go:build !windows

package service

import (
	"context"
	"os"
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
	// Keep execute so we can still stat / read files inside.
	if err := os.Chmod(pendingDir, 0o500); err != nil {
		t.Fatalf("chmod pending dir: %v", err)
	}
	t.Cleanup(func() {
		// Restore write permission so the test's own cleanup
		// of the temp directory succeeds.
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

	// The markers are still on disk because Delete failed.
	// Restore permissions locally for the List call.
	if err := os.Chmod(pendingDir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	remaining, _ := launcher.List(repo)
	if len(remaining) != 2 {
		t.Errorf("both markers should still be on disk after delete failures, got %v", remaining)
	}

	// Per-invocation skip set: a delete failure adds the
	// marker to the skip set, so each is tried exactly once.
	// More than two calls means the skip set regressed.
	if got := len(runner.calls()); got != 2 {
		t.Errorf("runner invoked %d times; expected exactly 2 (one per marker)", got)
	}
}
