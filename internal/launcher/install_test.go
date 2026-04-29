//go:build darwin

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests exercise enable and disable with isolated HOME,
// SEMANTICA_HOME, and fake launchctl.

func setupInstallEnv(t *testing.T) (home, semHome string) {
	t.Helper()
	home = t.TempDir()
	semHome = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SEMANTICA_HOME", semHome)
	return home, semHome
}

// fakeBinary creates an executable file that passes Enable's binary checks.
func fakeBinary(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	path := filepath.Join(dir, "semantica")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestEnable_InstallsPlistAndSettingsAndBootstraps(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Enable is macOS-specific; ErrUnsupportedOS path is covered elsewhere")
	}
	home, _ := setupInstallEnv(t)
	_, argvLog := writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	result, err := Enable(context.Background(), bin)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Result fields should point at the installed state.
	wantPlist := filepath.Join(home, "Library", "LaunchAgents", "sh.semantica.worker.plist")
	if result.PlistPath != wantPlist {
		t.Errorf("result.PlistPath = %q, want %q", result.PlistPath, wantPlist)
	}
	if result.Reinstalled {
		t.Errorf("first Enable must not report Reinstalled=true")
	}

	// The installed plist should contain the expected paths and label.
	body, err := os.ReadFile(wantPlist)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}
	want := []string{bin, "sh.semantica.worker", "worker-launcher.log", "<key>RunAtLoad</key>"}
	for _, s := range want {
		if !strings.Contains(string(body), s) {
			t.Errorf("installed plist missing %q; body:\n%s", s, body)
		}
	}

	// Enable should call bootout first, then bootstrap.
	lines := readArgvLines(t, argvLog)
	if len(lines) != 2 {
		t.Fatalf("launchctl invocations = %d, want 2 (bootout + bootstrap): %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "bootout ") {
		t.Errorf("first launchctl call = %q, want bootout", lines[0])
	}
	if !strings.HasPrefix(lines[1], "bootstrap ") || !strings.Contains(lines[1], wantPlist) {
		t.Errorf("second launchctl call = %q, want bootstrap of %s", lines[1], wantPlist)
	}

	// Settings should record the enabled state and plist path.
	settings, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if !settings.Launcher.Enabled {
		t.Errorf("settings.Launcher.Enabled = false after Enable")
	}
	if settings.Launcher.InstalledPlistPath != wantPlist {
		t.Errorf("settings plist path = %q, want %q",
			settings.Launcher.InstalledPlistPath, wantPlist)
	}
	if settings.Launcher.InstalledAt == 0 {
		t.Errorf("settings.Launcher.InstalledAt must be set")
	}
}

func TestEnable_IdempotentOnAlreadyEnabledReRendersAndReBootstraps(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Enable is macOS-specific")
	}
	home, _ := setupInstallEnv(t)
	_, argvLog := writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	if _, err := Enable(context.Background(), bin); err != nil {
		t.Fatalf("first Enable: %v", err)
	}

	// Truncate the argv log so the second call is isolated.
	if err := os.WriteFile(argvLog, nil, 0o644); err != nil {
		t.Fatalf("truncate argv log: %v", err)
	}

	result, err := Enable(context.Background(), bin)
	if err != nil {
		t.Fatalf("second Enable: %v", err)
	}
	if !result.Reinstalled {
		t.Errorf("second Enable must report Reinstalled=true (launchd reported a loaded service)")
	}

	// The second Enable should bootout first, then bootstrap.
	lines := readArgvLines(t, argvLog)
	if len(lines) != 2 {
		t.Fatalf("second Enable launchctl invocations = %d, want 2: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "bootout ") {
		t.Errorf("second Enable call[0] = %q, want bootout", lines[0])
	}
	if !strings.HasPrefix(lines[1], "bootstrap ") {
		t.Errorf("second Enable call[1] = %q, want bootstrap", lines[1])
	}
}

// Enable should recover when launchd state and settings disagree.
func TestEnable_IdempotentWhenSettingsDesyncedFromLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Enable is macOS-specific")
	}
	home, semHome := setupInstallEnv(t)
	_, argvLog := writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	// Seed a loaded launchd state but remove the settings file.
	if _, err := Enable(context.Background(), bin); err != nil {
		t.Fatalf("seed Enable: %v", err)
	}
	if err := os.Remove(filepath.Join(semHome, "settings.json")); err != nil {
		t.Fatalf("delete settings: %v", err)
	}
	if IsEnabled() {
		t.Fatalf("precondition failed: settings say enabled after deletion")
	}

	// Reset the argv log so the retry is isolated.
	if err := os.WriteFile(argvLog, nil, 0o644); err != nil {
		t.Fatalf("truncate argv log: %v", err)
	}

	// Retry Enable and expect a clean reload.
	result, err := Enable(context.Background(), bin)
	if err != nil {
		t.Fatalf("Enable retry after settings desync: %v", err)
	}
	if !result.Reinstalled {
		t.Errorf("Reinstalled must be true because launchd had the service loaded")
	}
	lines := readArgvLines(t, argvLog)
	if len(lines) != 2 {
		t.Fatalf("retry launchctl invocations = %d, want 2 (bootout + bootstrap): %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "bootout ") {
		t.Errorf("retry call[0] = %q, want bootout", lines[0])
	}
	if !strings.HasPrefix(lines[1], "bootstrap ") {
		t.Errorf("retry call[1] = %q, want bootstrap", lines[1])
	}
}

// "No such process" should be treated as a fresh-install bootout result.
func TestEnable_TreatsBootoutNoSuchProcessAsFreshInstall(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Enable is macOS-specific")
	}
	home, _ := setupInstallEnv(t)
	// Fake launchctl that returns the not-loaded bootout wording.
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	script := `#!/bin/bash
printf '%s\n' "$*" >> ` + argvLog + `
if [[ "$1" == "bootout" ]]; then
  echo "Boot-out failed: 3: No such process" >&2
  exit 3
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake launchctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bin := fakeBinary(t, home)
	result, err := Enable(context.Background(), bin)
	if err != nil {
		t.Fatalf("Enable should treat bootout no-such-process as soft success; got: %v", err)
	}
	if result.Reinstalled {
		t.Errorf("Reinstalled must be false when bootout reported not loaded")
	}

	// Expect bootout, then bootstrap.
	lines, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(lines), "\n"), "\n")
	if len(got) != 2 {
		t.Fatalf("expected 2 launchctl calls, got %d: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "bootout ") {
		t.Errorf("first call = %q, want bootout", got[0])
	}
	if !strings.HasPrefix(got[1], "bootstrap ") {
		t.Errorf("second call = %q, want bootstrap", got[1])
	}
}

func TestEnable_RejectsRelativeBinaryPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	_, err := Enable(context.Background(), "relative/semantica")
	if err == nil {
		t.Fatal("expected validation error for relative binary path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected 'absolute' in error, got %v", err)
	}
}

func TestEnable_RejectsMissingBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	_, err := Enable(context.Background(), "/no/such/binary")
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
}

func TestEnable_ReturnsUnsupportedOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test documents behavior on non-darwin")
	}
	setupInstallEnv(t)
	_, err := Enable(context.Background(), "/anything")
	if err != ErrUnsupportedOS {
		t.Errorf("Enable on non-darwin returned %v, want ErrUnsupportedOS", err)
	}
}

func TestDisable_RemovesPlistAndClearsSettings(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	home, _ := setupInstallEnv(t)
	_, argvLog := writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	if _, err := Enable(context.Background(), bin); err != nil {
		t.Fatalf("seed enable: %v", err)
	}
	plistPath, err := PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("seed plist missing: %v", err)
	}

	// Truncate argv log so the disable's launchctl argv is
	// what we read.
	if err := os.WriteFile(argvLog, nil, 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	result, err := Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !result.WasEnabled {
		t.Errorf("WasEnabled = false despite seeded enable")
	}
	if result.RemovedPlistPath != plistPath {
		t.Errorf("RemovedPlistPath = %q, want %q", result.RemovedPlistPath, plistPath)
	}

	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist should be removed; stat=%v", err)
	}

	argv := readArgv(t, argvLog)
	if !strings.Contains(argv, "bootout") {
		t.Errorf("expected bootout in launchctl argv, got %q", argv)
	}

	settings, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if settings.Launcher.Enabled {
		t.Errorf("settings.Launcher.Enabled = true after Disable")
	}
}

func TestDisable_OnCleanStateIsNoopAndReportsWasEnabledFalse(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)

	// The stateful fake returns "service not found" for the
	// bootout of a label that has never been bootstrapped,
	// which is what real launchctl does; Disable must treat
	// that as success.
	writeStatefulFakeLaunchctl(t)

	result, err := Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable on clean state: %v", err)
	}
	if result.WasEnabled {
		t.Error("WasEnabled should be false on a clean state")
	}
	if result.RemovedPlistPath != "" {
		t.Errorf("RemovedPlistPath should be empty when no file existed, got %q",
			result.RemovedPlistPath)
	}
}

// Disable must surface unexpected bootout failures.
func TestDisable_SurfacesUnexpectedBootoutError(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	writeFakeLaunchctl(t, 9, "Unrecognized target specifier")

	_, err := Disable(context.Background())
	if err == nil {
		t.Fatal("expected error when launchctl bootout returns an unknown failure")
	}
	if !strings.Contains(err.Error(), "bootout") {
		t.Errorf("expected 'bootout' in error, got %v", err)
	}
}

func TestDisable_ReturnsUnsupportedOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	_, err := Disable(context.Background())
	if err != ErrUnsupportedOS {
		t.Errorf("Disable on non-darwin returned %v, want ErrUnsupportedOS", err)
	}
}
