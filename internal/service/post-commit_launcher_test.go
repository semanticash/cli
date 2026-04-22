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

// These tests cover launcher dispatch from the post-commit hook.

// writeFakeLaunchctlForService installs a local fake launchctl.
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

// enableLauncherInSettings makes launcher.IsEnabled return true.
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

// setupLauncherDispatchEnv creates an isolated repo and global home.
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
	// No settings file means launcher.IsEnabled() returns false.

	err := dispatchViaLauncher(context.Background(), "cp-1", "commit-1", repo)
	if !errors.Is(err, ErrLauncherNotEnabled) {
		t.Fatalf("expected ErrLauncherNotEnabled, got %v", err)
	}

	// Disabled dispatch should be side-effect free.
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

	// The marker should be written at the canonical path.
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

	// kickstart should target the canonical domain without -k.
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

	// The marker should stay on disk for a later drain.
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

	// Make the repo's .semantica directory read-only so marker creation fails.
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

	// If marker creation fails, launchd must not be kicked.
	if _, err := os.Stat(argvLog); err == nil {
		body, _ := os.ReadFile(argvLog)
		if len(bytes.TrimSpace(body)) > 0 {
			t.Errorf("launchctl was invoked after marker-write failure; argv log:\n%s", body)
		}
	}
}
