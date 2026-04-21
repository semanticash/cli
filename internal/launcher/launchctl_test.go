//go:build !windows

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests use a Bourne-shell fake launchctl dropped in a temp
// directory that is prepended to PATH for the duration of the
// subtest. The fake writes its argv to a file for the test to
// inspect and exits with a code controlled by the test. This
// exercises the wrapper's shell-out and argv composition without
// touching the real launchd.
//
// The build tag excludes Windows because the fake is a bash script.
// macOS and Linux both honor the same exec + PATH semantics, so the
// argv-shape tests run identically on both; the platform-specific
// runtime guard inside the wrappers is covered by a dedicated test
// when the host is not macOS.

func writeFakeLaunchctl(t *testing.T, exitCode int, stderrMsg string) (dir, argvLogPath string) {
	t.Helper()
	dir = t.TempDir()
	argvLogPath = filepath.Join(dir, "argv.log")

	script := fmt.Sprintf(`#!/bin/bash
# Fake launchctl used by internal/launcher tests. Logs the received
# argv and exits with the configured code.
printf '%%s\n' "$*" > %q
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
	return dir, argvLogPath
}

func readArgv(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	return strings.TrimRight(string(b), "\n")
}

func skipIfNotDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl wrappers are only active on darwin; this test exercises the live path via a fake launchctl")
	}
}

func TestBootstrap_SuccessInvokesCorrectArgv(t *testing.T) {
	skipIfNotDarwin(t)
	_, argvLog := writeFakeLaunchctl(t, 0, "")

	err := Bootstrap(context.Background(), "gui/501", "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got := readArgv(t, argvLog)
	want := "bootstrap gui/501 /Users/test/Library/LaunchAgents/sh.semantica.worker.plist"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestBootstrap_NonZeroExitReturnsTypedError(t *testing.T) {
	skipIfNotDarwin(t)
	writeFakeLaunchctl(t, 5, "Bootstrap failed: 5: Input/output error")

	err := Bootstrap(context.Background(), "gui/501", "/ignored.plist")
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}

	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if le.ExitCode != 5 {
		t.Errorf("ExitCode = %d, want 5", le.ExitCode)
	}
	if le.Subcommand != "bootstrap" {
		t.Errorf("Subcommand = %q, want bootstrap", le.Subcommand)
	}
	if !strings.Contains(le.Stderr, "Input/output error") {
		t.Errorf("Stderr = %q, expected it to contain the launchctl message", le.Stderr)
	}
	if !strings.Contains(le.Error(), "exit 5") {
		t.Errorf("Error() = %q, expected to mention exit code", le.Error())
	}
}

func TestBootout_SuccessInvokesCorrectArgv(t *testing.T) {
	skipIfNotDarwin(t)
	_, argvLog := writeFakeLaunchctl(t, 0, "")

	err := Bootout(context.Background(), "gui/501/sh.semantica.worker")
	if err != nil {
		t.Fatalf("Bootout: %v", err)
	}

	got := readArgv(t, argvLog)
	want := "bootout gui/501/sh.semantica.worker"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestKickstart_SuccessInvokesCorrectArgvWithoutDashK(t *testing.T) {
	skipIfNotDarwin(t)
	_, argvLog := writeFakeLaunchctl(t, 0, "")

	err := Kickstart(context.Background(), "gui/501/sh.semantica.worker")
	if err != nil {
		t.Fatalf("Kickstart: %v", err)
	}

	got := readArgv(t, argvLog)
	// The -k flag is deliberately absent: an already-running
	// worker should not be killed and restarted on every commit.
	want := "kickstart gui/501/sh.semantica.worker"
	if got != want {
		t.Errorf("argv = %q, want %q (note: -k must NOT be present)", got, want)
	}
	if strings.Contains(got, " -k ") || strings.HasSuffix(got, " -k") {
		t.Errorf("argv must not include -k flag, got %q", got)
	}
}

func TestIsLoaded_ZeroExitMeansLoaded(t *testing.T) {
	skipIfNotDarwin(t)
	_, argvLog := writeFakeLaunchctl(t, 0, "")

	loaded, err := IsLoaded(context.Background(), "gui/501/sh.semantica.worker")
	if err != nil {
		t.Fatalf("IsLoaded: %v", err)
	}
	if !loaded {
		t.Error("expected loaded=true when launchctl print exits 0")
	}

	got := readArgv(t, argvLog)
	want := "print gui/501/sh.semantica.worker"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// launchctl print for an unregistered label returns non-zero with
// a stable "Could not find service" message. IsLoaded should
// translate that specific verdict into a clean (false, nil).
func TestIsLoaded_ServiceNotFoundMeansNotLoaded(t *testing.T) {
	skipIfNotDarwin(t)
	writeFakeLaunchctl(t, 113, "Could not find service \"sh.semantica.worker\" in domain for port")

	loaded, err := IsLoaded(context.Background(), "gui/501/sh.semantica.worker")
	if err != nil {
		t.Fatalf("IsLoaded returned non-nil error on not-loaded: %v", err)
	}
	if loaded {
		t.Error("expected loaded=false when launchctl print reports service not found")
	}
}

// An alternate wording ("service not found") must also be
// recognized. Apple has shipped both forms across macOS releases.
func TestIsLoaded_AlternateNotFoundWordingMeansNotLoaded(t *testing.T) {
	skipIfNotDarwin(t)
	writeFakeLaunchctl(t, 3, "Service not found.")

	loaded, err := IsLoaded(context.Background(), "gui/501/sh.semantica.worker")
	if err != nil {
		t.Fatalf("IsLoaded returned non-nil error on alt wording: %v", err)
	}
	if loaded {
		t.Error("expected loaded=false on alternate not-found wording")
	}
}

// Any other non-zero exit (malformed target, permission denied,
// generic launchctl error) must surface as an error rather than
// silently flattening to "not loaded". Masking these would hide
// bugs in callers that compose the wrapper into enable/disable.
func TestIsLoaded_UnexpectedLaunchctlErrorSurfaces(t *testing.T) {
	skipIfNotDarwin(t)
	writeFakeLaunchctl(t, 9, "Unrecognized target specifier: gui")

	loaded, err := IsLoaded(context.Background(), "gui/malformed target")
	if err == nil {
		t.Fatal("expected error for unrecognized-target failure, got nil")
	}
	if loaded {
		t.Error("loaded must be false on error")
	}
	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if le.ExitCode != 9 {
		t.Errorf("ExitCode = %d, want 9", le.ExitCode)
	}
	if !strings.Contains(le.Stderr, "Unrecognized target") {
		t.Errorf("expected stderr to contain original launchctl message, got %q", le.Stderr)
	}
}

// When launchctl itself is missing from PATH, the error should
// not masquerade as a typed *Error with ExitCode=0. Callers need
// to distinguish "launchctl said no" from "launchctl could not be
// invoked at all".
func TestBootstrap_LaunchctlMissingSurfacesExecError(t *testing.T) {
	skipIfNotDarwin(t)
	// Point PATH at an empty dir so exec.LookPath fails.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	err := Bootstrap(context.Background(), "gui/501", "/ignored.plist")
	if err == nil {
		t.Fatal("expected error when launchctl is absent from PATH, got nil")
	}
	var le *Error
	if errors.As(err, &le) {
		t.Errorf("missing launchctl must not surface as *Error (which implies a real exit code); got %+v", le)
	}
}

// Exercised on all non-darwin hosts: every wrapper must fail fast
// with ErrUnsupportedOS so a caller on Linux or Windows never ends
// up exec'ing a nonexistent launchctl or otherwise corrupting state.
func TestAllWrappers_ReturnUnsupportedOSOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test documents behavior on non-darwin hosts")
	}

	ctx := context.Background()
	if err := Bootstrap(ctx, "gui/0", "/x"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Bootstrap: got %v, want ErrUnsupportedOS", err)
	}
	if err := Bootout(ctx, "gui/0/x"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Bootout: got %v, want ErrUnsupportedOS", err)
	}
	if err := Kickstart(ctx, "gui/0/x"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Kickstart: got %v, want ErrUnsupportedOS", err)
	}
	if _, err := IsLoaded(ctx, "gui/0/x"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("IsLoaded: got %v, want ErrUnsupportedOS", err)
	}
}
