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

// IsRegistered must report registration state, not running state.
// Type=oneshot units return to "inactive" between kicks, which is
// the steady state - not a problem. The previous IsActive
// implementation used systemctl is-active and would report false
// for any idle unit, making Status render the drift hint
// "settings say enabled, but the OS daemon manager has no loaded
// service" on every status check between worker runs.
//
// The test wires a fake systemctl that returns LoadState=loaded
// from `systemctl --user show`, mimicking the steady state of an
// installed unit between kicks. IsRegistered must return true.
func TestIsRegistered_IdleUnitCountsAsLoaded(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	// Fake systemctl: prints "loaded" to stdout for show
	// LoadState queries, exits 0. Logs argv for assertion below.
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$*" >> %q
case "$2" in
  show)
    echo "loaded"
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

	// Pin the systemctl probe used: must be `show LoadState`,
	// not `is-active`. Using is-active would re-introduce the
	// false-on-idle bug for Type=oneshot units.
	logBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if !strings.Contains(string(logBytes), "--user show") {
		t.Errorf("IsRegistered must probe via `show`, got argv:\n%s", string(logBytes))
	}
	if strings.Contains(string(logBytes), "is-active") {
		t.Errorf("IsRegistered must NOT use `is-active` (returns false for idle Type=oneshot units), got argv:\n%s", string(logBytes))
	}
}

// TestInstall_ReinstalledFlagUsesRegistrationProbe pins that the
// Reinstalled detection on Linux uses isUnitRegistered, NOT
// isUnitActive. Type=oneshot units return to inactive between
// kicks, so probing with is-active would falsely report
// Reinstalled=false on every reinstall of an idle but registered
// unit. Sibling regression to the Status fix that switched to
// registration semantics.
func TestInstall_ReinstalledFlagUsesRegistrationProbe(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	// Fake systemctl: succeeds for every operation. `show
	// LoadState` returns "loaded" so Install sees a previously-
	// registered unit. is-active is configured to LIE - return
	// "inactive" exit 3 - so a regression that probes with
	// is-active instead of show would read false here and the
	// Reinstalled assertion would fail.
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

	// Pin the systemctl probe used: registration check must go
	// through `show`, not `is-active`.
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
