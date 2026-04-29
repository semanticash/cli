//go:build linux

package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Disable must still remove the unit file and clear settings even
// when the systemd user instance is unavailable.
func TestDisable_BestEffortWhenSystemctlAlwaysFails(t *testing.T) {
	xdg := t.TempDir()
	semHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SEMANTICA_HOME", semHome)

	// Fake systemctl that fails every invocation. show-environment
	// fails (would have blocked Disable in the buggy version);
	// stop and daemon-reload also fail. Despite all that, Disable
	// must still remove the unit file.
	writeFakeSystemctl(t, 1, "Failed to connect to bus")

	// Seed the unit file as if a previous Enable had run.
	unitPath, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	if err := os.WriteFile(unitPath, []byte("# stub unit\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	// Seed settings to record the install path.
	if err := WriteSettings(UserSettings{
		Launcher: LauncherSettings{
			Enabled:           true,
			InstalledUnitPath: unitPath,
			InstalledAt:       1,
		},
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	result, err := Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable must not error in degraded environments, got: %v", err)
	}
	if errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Disable must NOT return ErrUnsupportedOS on linux - backend exists, just runtime is degraded")
	}
	if !result.WasEnabled {
		t.Errorf("WasEnabled = false, want true")
	}
	if result.RemovedUnitPath != unitPath {
		t.Errorf("RemovedUnitPath = %q, want %q", result.RemovedUnitPath, unitPath)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file still on disk after Disable: stat=%v", err)
	}

	// Settings must be cleared so a re-enable starts from a clean
	// state.
	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings post-Disable: %v", err)
	}
	if s.Launcher.Enabled {
		t.Errorf("Launcher.Enabled = true after Disable; settings not cleared")
	}
	if s.Launcher.InstalledUnitPath != "" {
		t.Errorf("InstalledUnitPath = %q after Disable; settings not cleared", s.Launcher.InstalledUnitPath)
	}
}

// TestInstall_ReachabilityProbeFailsWithClearError pins that the
// systemd reachability probe lives in Install (not newManager) and
// surfaces a clear, actionable error when the user manager is
// unreachable. The error is NOT ErrUnsupportedOS - Linux supports
// the launcher; this is a runtime environment issue.
func TestInstall_ReachabilityProbeFailsWithClearError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	writeFakeSystemctl(t, 1, "Failed to connect to bus")

	// Need a binary path that exists for the path-validation step.
	binaryPath := filepath.Join(t.TempDir(), "semantica")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	_, err := Enable(context.Background(), binaryPath)
	if err == nil {
		t.Fatal("Enable must fail when systemd user instance is unreachable")
	}
	if errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Enable error should NOT be ErrUnsupportedOS - Linux supports the backend; got: %v", err)
	}
	if msg := err.Error(); !contains(msg, "systemd user instance not reachable") {
		t.Errorf("error message should point at the systemd environment problem, got: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
