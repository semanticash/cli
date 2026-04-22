//go:build !windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/launcher"
)

// These integration tests exercise launcher dispatch and drain together.

// setupLauncherIntegrationEnv creates the repo and global state for these tests.
func setupLauncherIntegrationEnv(t *testing.T, repoRoots ...string) {
	t.Helper()

	home := t.TempDir()
	base := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SEMANTICA_HOME", base)

	// Register each repo so the drain loop can discover it.
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	ctx := context.Background()
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("broker.Open: %v", err)
	}
	defer func() { _ = broker.Close(bh) }()
	for _, root := range repoRoots {
		if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
			t.Fatalf("mkdir %s/.semantica: %v", root, err)
		}
		if err := broker.Register(ctx, bh, root, root); err != nil {
			t.Fatalf("broker.Register %s: %v", root, err)
		}
	}

	// Seed the launcher-enabled flag so dispatchViaLauncher runs.
	s := launcher.UserSettings{
		Launcher: launcher.LauncherSettings{
			Enabled:            true,
			InstalledPlistPath: "/dummy/path.plist",
			InstalledAt:        1,
		},
	}
	if err := launcher.WriteSettings(s); err != nil {
		t.Fatalf("seed launcher settings: %v", err)
	}
}

// A dispatched marker should be discovered and processed by the drain loop.
func TestIntegration_Launcher_DispatchAndDrainProcessSingleCommit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("dispatchViaLauncher's kickstart is macOS-only")
	}
	repo := t.TempDir()
	setupLauncherIntegrationEnv(t, repo)
	writeFakeLaunchctlForService(t, 0, "")

	// Dispatch writes the marker and kicks the fake launchd agent.
	err := dispatchViaLauncher(context.Background(), "cp-integration", "commit-integ", repo)
	if err != nil {
		t.Fatalf("dispatchViaLauncher: %v", err)
	}
	if _, err := os.Stat(launcher.MarkerPath(repo, "cp-integration")); err != nil {
		t.Fatalf("marker missing after dispatch: %v", err)
	}

	// Drain with a recording runner so the handoff can be inspected.
	var runner recordingRunner
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}

	calls := runner.calls()
	if len(calls) != 1 {
		t.Fatalf("runner called %d times, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.CheckpointID != "cp-integration" ||
		got.CommitHash != "commit-integ" ||
		got.RepoRoot != repo {
		t.Errorf("runner received %+v; want checkpoint cp-integration, commit commit-integ, repo %s",
			got, repo)
	}

	// The marker should be deleted after a successful run.
	if _, err := os.Stat(launcher.MarkerPath(repo, "cp-integration")); !os.IsNotExist(err) {
		t.Errorf("marker should be deleted after successful drain, stat=%v", err)
	}
}

// A burst of dispatches across repos should process each marker once.
func TestIntegration_Launcher_BurstOfDispatchesAcrossReposProcessesEachExactlyOnce(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	repoA := t.TempDir()
	repoB := t.TempDir()
	setupLauncherIntegrationEnv(t, repoA, repoB)
	writeFakeLaunchctlForService(t, 0, "")

	type dispatch struct {
		repo, ckpt, commit string
	}
	dispatches := []dispatch{
		{repoA, "ck-a1", "commit-a1"},
		{repoA, "ck-a2", "commit-a2"},
		{repoB, "ck-b1", "commit-b1"},
	}

	// Dispatch three markers back-to-back.
	for _, d := range dispatches {
		if err := dispatchViaLauncher(context.Background(), d.ckpt, d.commit, d.repo); err != nil {
			t.Fatalf("dispatch %+v: %v", d, err)
		}
	}

	var runner recordingRunner
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}

	calls := runner.calls()
	if len(calls) != len(dispatches) {
		t.Fatalf("runner called %d times, want %d: %+v", len(calls), len(dispatches), calls)
	}

	// Every dispatched (checkpoint, repo) pair should appear once.
	seen := map[string]int{}
	for _, c := range calls {
		key := c.CheckpointID + "@" + c.RepoRoot
		seen[key]++
	}
	for _, d := range dispatches {
		key := d.ckpt + "@" + d.repo
		if seen[key] != 1 {
			t.Errorf("checkpoint %s in repo %s ran %d times, want 1", d.ckpt, d.repo, seen[key])
		}
	}

	// Neither repo should keep markers after the drain.
	for _, repo := range []string{repoA, repoB} {
		remaining, _ := launcher.List(repo)
		if len(remaining) != 0 {
			t.Errorf("repo %s has %d leftover markers: %v", repo, len(remaining), remaining)
		}
	}
}

// A kickstart failure should leave the marker for a later drain.
func TestIntegration_Launcher_KickstartFailureLeavesMarkerForLaterDrain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	repo := t.TempDir()
	setupLauncherIntegrationEnv(t, repo)
	// Fake launchctl that fails every call.
	writeFakeLaunchctlForService(t, 5, "Kickstart failed")

	// Dispatch fails but leaves the marker on disk.
	err := dispatchViaLauncher(context.Background(), "cp-kick-fail", "commit-kf", repo)
	if err == nil {
		t.Fatal("expected dispatch error from failing kickstart")
	}
	if _, err := os.Stat(launcher.MarkerPath(repo, "cp-kick-fail")); err != nil {
		t.Fatalf("marker must remain on disk after kickstart failure, stat=%v", err)
	}

	// A later drain should find and process the marker.
	var runner recordingRunner
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	calls := runner.calls()
	if len(calls) != 1 || calls[0].CheckpointID != "cp-kick-fail" {
		t.Errorf("drain did not pick up the stranded marker: calls=%+v", calls)
	}
	if _, err := os.Stat(launcher.MarkerPath(repo, "cp-kick-fail")); !os.IsNotExist(err) {
		t.Errorf("marker should be deleted after eventual drain, stat=%v", err)
	}
}
