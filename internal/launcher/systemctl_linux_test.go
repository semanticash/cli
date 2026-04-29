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

// These tests use a fake systemctl on PATH so the wrapper can be
// exercised without touching the real systemd user instance.

// writeFakeSystemctl installs a fake `systemctl` binary on PATH
// that exits with the configured code and writes the received
// argv to argvLogPath.
func writeFakeSystemctl(t *testing.T, exitCode int, stderrMsg string) (dir, argvLogPath string) {
	t.Helper()
	dir = t.TempDir()
	argvLogPath = filepath.Join(dir, "argv.log")

	script := fmt.Sprintf(`#!/bin/bash
# Fake systemctl used by internal/launcher tests. Logs the received
# argv and exits with the configured code.
printf '%%s\n' "$*" > %q
if [[ -n %q ]]; then
  printf '%%s\n' %q >&2
fi
exit %d
`, argvLogPath, stderrMsg, stderrMsg, exitCode)

	path := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}

	orig := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
	return dir, argvLogPath
}

func readSystemctlArgv(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	return strings.TrimRight(string(b), "\n")
}

func TestDaemonReload_InvokesCorrectArgv(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	if err := daemonReload(context.Background()); err != nil {
		t.Fatalf("daemonReload: %v", err)
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user daemon-reload"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestStartUnit_PassesNoBlockAndUnitName(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	if err := startUnit(context.Background(), "sh.semantica.worker.service"); err != nil {
		t.Fatalf("startUnit: %v", err)
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user start --no-block sh.semantica.worker.service"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestStopUnit_InvokesCorrectArgv(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	if err := stopUnit(context.Background(), "sh.semantica.worker.service"); err != nil {
		t.Fatalf("stopUnit: %v", err)
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user stop sh.semantica.worker.service"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// is-active exits 0 when the unit is active. Wrapper must report
// (true, nil).
func TestIsUnitActive_ZeroExitMeansActive(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	active, err := isUnitActive(context.Background(), "sh.semantica.worker.service")
	if err != nil {
		t.Fatalf("isUnitActive: %v", err)
	}
	if !active {
		t.Error("expected active=true when systemctl exits 0")
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user is-active --quiet sh.semantica.worker.service"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// is-active exits 3 when the unit exists but is inactive. Wrapper
// must flatten this to (false, nil) - same shape launchctl uses
// for its "service not found" wording.
func TestIsUnitActive_ExitThreeMeansInactive(t *testing.T) {
	writeFakeSystemctl(t, 3, "")

	active, err := isUnitActive(context.Background(), "sh.semantica.worker.service")
	if err != nil {
		t.Fatalf("isUnitActive returned error on exit 3: %v", err)
	}
	if active {
		t.Error("expected active=false when systemctl exits 3")
	}
}

// Other non-zero exits (e.g., systemctl misuse, missing
// permissions) must propagate as a typed error rather than
// flattening to a boolean - exit codes outside the documented
// set are operational problems, not state signals.
func TestIsUnitActive_OtherNonZeroExitsPropagate(t *testing.T) {
	writeFakeSystemctl(t, 5, "Failed to connect to bus")

	_, err := isUnitActive(context.Background(), "sh.semantica.worker.service")
	if err == nil {
		t.Fatal("expected error on exit 5, got nil")
	}
	var se *systemctlError
	if !errors.As(err, &se) {
		t.Fatalf("expected *systemctlError, got %T: %v", err, err)
	}
	if se.ExitCode != 5 {
		t.Errorf("ExitCode = %d, want 5", se.ExitCode)
	}
	if !strings.Contains(se.Stderr, "Failed to connect to bus") {
		t.Errorf("Stderr = %q, expected systemctl message preserved", se.Stderr)
	}
}

// userManagerReachable uses show-environment, NOT is-system-running.
// is-system-running returns non-zero for `degraded`/`starting`/
// `maintenance` even when the user manager is perfectly usable;
// keying on its exit code would falsely classify any host with one
// failed user unit as "no launcher backend". This test pins the
// chosen probe.
func TestUserManagerReachable_UsesShowEnvironment(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	if err := userManagerReachable(context.Background()); err != nil {
		t.Fatalf("userManagerReachable: %v", err)
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user show-environment"
	if got != want {
		t.Errorf("argv = %q, want %q (must NOT use is-system-running)", got, want)
	}
}

func TestUserManagerReachable_NonZeroExitFails(t *testing.T) {
	writeFakeSystemctl(t, 1, "Failed to connect to user manager")

	err := userManagerReachable(context.Background())
	if err == nil {
		t.Fatal("expected error when systemctl exits non-zero, got nil")
	}
	var se *systemctlError
	if !errors.As(err, &se) {
		t.Fatalf("expected *systemctlError, got %T: %v", err, err)
	}
	if se.Subcommand != "show-environment" {
		t.Errorf("Subcommand = %q, want show-environment", se.Subcommand)
	}
}

// Kickstart on Linux honors the caller-supplied target and maps
// to systemctl --user start --no-block <unit>. Same source-compat
// contract as the darwin Kickstart.
func TestKickstart_HonorsCallerSuppliedTargetOnLinux(t *testing.T) {
	_, argvLog := writeFakeSystemctl(t, 0, "")

	const callerTarget = "some.other.label.service"
	if err := Kickstart(context.Background(), callerTarget); err != nil {
		t.Fatalf("Kickstart: %v", err)
	}

	got := readSystemctlArgv(t, argvLog)
	want := "--user start --no-block " + callerTarget
	if got != want {
		t.Errorf("systemctl argv = %q, want %q", got, want)
	}
}
