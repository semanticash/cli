//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Simulate an unavailable systemd user instance.
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

// A loaded oneshot service is healthy while idle when its timer is
// enabled and active.
func TestIsRegistered_IdleUnitCountsAsLoaded(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	// Log each probe while reporting a healthy service and timer.
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$*" >> %q
case "$2" in
  show)
    echo "loaded"
    ;;
  is-enabled)
    echo "enabled"
    ;;
esac
exit 0
`, argvLog)
	scriptPath := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("seed fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, err := newManager()
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	registered, err := m.IsRegistered(context.Background())
	if err != nil {
		t.Fatalf("IsRegistered: %v", err)
	}
	if !registered {
		t.Errorf("LoadState=loaded must report registered=true")
	}

	// The service uses LoadState; the waiting timer uses activity.
	logBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := string(logBytes)
	if !strings.Contains(argv, "--user show") {
		t.Errorf("IsRegistered must probe the service via `show`, got argv:\n%s", argv)
	}
	if strings.Contains(argv, "is-active --quiet "+UnitTarget()) {
		t.Errorf("IsRegistered must NOT use `is-active` for the service, got argv:\n%s", argv)
	}
	if !strings.Contains(argv, "is-active --quiet "+TimerTarget()) {
		t.Errorf("IsRegistered must probe the timer via `is-active`, got argv:\n%s", argv)
	}
}

// Reinstallation checks service registration rather than activity.
func TestInstall_ReinstalledFlagUsesRegistrationProbe(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	// Report the service as loaded but inactive between runs.
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$*" >> %q
case "$2" in
  show)
    echo "loaded"
    exit 0
    ;;
  is-active)
    exit 3
    ;;
esac
exit 0
`, argvLog)
	scriptPath := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("seed fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	binaryPath := filepath.Join(t.TempDir(), "semantica")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	result, err := Enable(context.Background(), binaryPath)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !result.Reinstalled {
		t.Errorf("Reinstalled=false even though `show LoadState=loaded` reported a prior registration; probe is using the wrong systemctl subcommand")
	}

	// Registration must use LoadState, not activity.
	logBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if !strings.Contains(string(logBytes), "--user show") {
		t.Errorf("Reinstalled probe must use `show`, got argv:\n%s", string(logBytes))
	}
	if strings.Contains(string(logBytes), "is-active") {
		t.Errorf("Reinstalled probe must NOT use `is-active` (false on idle Type=oneshot units), got argv:\n%s", string(logBytes))
	}
}

// Install reports an unreachable user manager as an environment error.
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

// A disabled or inactive timer makes the launcher unhealthy.
func TestIsRegistered_TimerHealthGates(t *testing.T) {
	cases := []struct {
		name           string
		isEnabledState string
		isEnabledExit  int
		isActiveExit   int
		want           bool
	}{
		{"timer enabled and active", "enabled", 0, 0, true},
		{"timer disabled", "disabled", 1, 0, false},
		{"timer static (exit 0 but not enabled)", "static", 0, 0, false},
		{"timer inactive", "enabled", 0, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := fmt.Sprintf(`#!/bin/bash
case "$2" in
  show)
    echo "loaded"
    ;;
  is-enabled)
    echo %q
    exit %d
    ;;
  is-active)
    exit %d
    ;;
esac
exit 0
`, tc.isEnabledState, tc.isEnabledExit, tc.isActiveExit)
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			m, err := newManager()
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.IsRegistered(context.Background())
			if err != nil {
				t.Fatalf("IsRegistered: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsRegistered = %v, want %v", got, tc.want)
			}
		})
	}
}
