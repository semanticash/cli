//go:build !windows

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/launcher"
)

// These tests cover the post-commit hook's choice between the
// launcher dispatch path and the legacy detached-spawn path.
// dispatchViaLauncher is the function under test; spawnWorker's
// decision logic is a straight switch on its return value and
// is covered by inspection rather than by running a real
// detached child in tests (which would re-exec the test binary).

// writeFakeLaunchctlForService installs a Bourne-shell fake
// launchctl that records its argv and exits with the given
// code, analogous to internal/launcher's test helper but local
// so this package does not depend on launcher's test-only code.
func writeFakeLaunchctlForService(t *testing.T, exitCode int, stderrMsg string) (argvLogPath string) {
	t.Helper()
	dir := t.TempDir()
	argvLogPath = filepath.Join(dir, "argv.log")

	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$*" >> %q
if [[ -n %q ]]; then
  printf '%%s\n' %q >&2
fi
exit %d
`, argvLogPath, stderrMsg, stderrMsg, exitCode)

	path := filepath.Join(dir, "launchctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake launchctl: %v", err)
	}
	orig := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
	return argvLogPath
}

// enableLauncherInSettings seeds ~/.semantica/settings.json so
// launcher.IsEnabled() returns true for the duration of the
// test. Does not touch launchd; tests that need to control
// launchctl behavior use writeFakeLaunchctlForService.
func enableLauncherInSettings(t *testing.T) {
	t.Helper()
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

// setupLauncherDispatchEnv isolates HOME, SEMANTICA_HOME, and a
// repo root the test can write a marker under.
func setupLauncherDispatchEnv(t *testing.T) (repo, semHome string) {
	t.Helper()
	home := t.TempDir()
	semHome = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SEMANTICA_HOME", semHome)
	repo = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir repo .semantica: %v", err)
	}
	return repo, semHome
}

func TestDispatchViaLauncher_NotEnabledReturnsSentinel(t *testing.T) {
	repo, _ := setupLauncherDispatchEnv(t)
	// No enableLauncherInSettings call; settings file is
	// absent so launcher.IsEnabled() returns false.

	err := dispatchViaLauncher(context.Background(), "cp-1", "commit-1", repo)
	if !errors.Is(err, ErrLauncherNotEnabled) {
		t.Fatalf("expected ErrLauncherNotEnabled, got %v", err)
	}

	// No marker should have been written when the launcher
	// is disabled: the function must exit before any side
	// effect.
	entries, _ := os.ReadDir(launcher.PendingDir(repo))
	if len(entries) != 0 {
		t.Errorf("disabled launcher must not write markers, got %v", entries)
	}
}

func TestDispatchViaLauncher_EnabledWritesMarkerAndKickstarts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kickstart is macOS-only; ErrUnsupportedOS surfaces as an error on other hosts")
	}
	repo, _ := setupLauncherDispatchEnv(t)
	enableLauncherInSettings(t)
	argvLog := writeFakeLaunchctlForService(t, 0, "")

	err := dispatchViaLauncher(context.Background(), "cp-1", "commit-1", repo)
	if err != nil {
		t.Fatalf("dispatchViaLauncher: %v", err)
	}

	// Marker present on disk at the canonical location.
	markerPath := launcher.MarkerPath(repo, "cp-1")
	data, readErr := os.ReadFile(markerPath)
	if readErr != nil {
		t.Fatalf("read marker: %v", readErr)
	}
	for _, want := range []string{"cp-1", "commit-1", repo} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("marker missing %q; body:\n%s", want, data)
		}
	}

	// launchctl kickstart was invoked against the canonical
	// domain target, without the -k flag.
	argv, _ := os.ReadFile(argvLog)
	line := strings.TrimRight(string(argv), "\n")
	wantPrefix := "kickstart " + launcher.DomainTarget()
	if line != wantPrefix {
		t.Errorf("launchctl argv = %q, want %q (no -k flag)", line, wantPrefix)
	}
}

func TestDispatchViaLauncher_KickstartFailureBubblesAndLeavesMarker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	repo, _ := setupLauncherDispatchEnv(t)
	enableLauncherInSettings(t)
	writeFakeLaunchctlForService(t, 5, "Kickstart failed: 5")

	err := dispatchViaLauncher(context.Background(), "cp-1", "commit-1", repo)
	if err == nil {
		t.Fatal("expected error when kickstart exits non-zero, got nil")
	}
	if !strings.Contains(err.Error(), "kickstart") {
		t.Errorf("expected 'kickstart' in error, got %v", err)
	}

	// The marker must still be on disk so a future successful
	// kickstart can process it; WorkerService.Run's checkpoint
	// idempotency makes the redundant retry cheap.
	if _, err := os.Stat(launcher.MarkerPath(repo, "cp-1")); err != nil {
		t.Errorf("marker should remain on disk after kickstart failure, stat=%v", err)
	}
}

func TestDispatchViaLauncher_MarkerWriteFailureSkipsKickstart(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX mode-bit enforcement")
	}
	repo, _ := setupLauncherDispatchEnv(t)
	enableLauncherInSettings(t)
	argvLog := writeFakeLaunchctlForService(t, 0, "")

	// Make the repo's .semantica directory read-only so
	// MkdirAll on the pending subdirectory, and the subsequent
	// marker write, fail with EACCES.
	semPath := filepath.Join(repo, ".semantica")
	if err := os.Chmod(semPath, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(semPath, 0o755) })

	err := dispatchViaLauncher(context.Background(), "cp-1", "commit-1", repo)
	if err == nil {
		t.Fatal("expected error when marker write fails, got nil")
	}
	if !strings.Contains(err.Error(), "marker") {
		t.Errorf("expected 'marker' in error, got %v", err)
	}

	// Critical invariant: if the marker cannot be written, we
	// must NOT kickstart. The launchd agent would find no
	// marker and exit, but the commit's work would have been
	// lost because the hook never fell back to the detached
	// spawn.
	if _, err := os.Stat(argvLog); err == nil {
		body, _ := os.ReadFile(argvLog)
		if len(bytes.TrimSpace(body)) > 0 {
			t.Errorf("launchctl was invoked after marker-write failure; argv log:\n%s", body)
		}
	}
}
