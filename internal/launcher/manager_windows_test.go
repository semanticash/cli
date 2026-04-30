//go:build windows

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnable_AcceptsWindowsAbsolutePath pins the cross-platform
// preflight contract: Enable's absoluteness check must use
// filepath.IsAbs (GOOS-aware), not the legacy isPOSIXAbsolute
// (POSIX-only). Without this, every os.Executable() value on
// Windows fails the preflight before the Task Scheduler backend
// is even reached, blocking the entire Windows launcher path.
//
// The test passes a non-existent absolute Windows path and asserts
// the rejection happens on the os.Stat step ("binary path not
// usable"), NOT on the absoluteness step ("must be absolute").
// That proves the absoluteness check accepts the path; the stat
// failure is an expected and clear separate concern.
func TestEnable_AcceptsWindowsAbsolutePath(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	const nonExistent = `C:\does\not\exist\semantica.exe`
	_, err := Enable(context.Background(), nonExistent)
	if err == nil {
		t.Fatal("expected Enable to fail with stat error; got nil")
	}
	if strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("Windows path was rejected as not-absolute; preflight is broken. err=%v", err)
	}
	if !strings.Contains(err.Error(), "binary path not usable") {
		t.Errorf("expected 'binary path not usable' (stat failure), got: %v", err)
	}
}

// Symmetric negative: a relative path must still be rejected on
// Windows (filepath.IsAbs returns false for `bin\foo.exe`).
func TestEnable_RejectsRelativeWindowsPath(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	_, err := Enable(context.Background(), `bin\semantica.exe`)
	if err == nil {
		t.Fatal("expected Enable to reject relative path; got nil")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("expected 'must be absolute' rejection, got: %v", err)
	}
}

// IsRegistered must report registration state, not running state.
// Task Scheduler tasks sit in "Ready" between executions, which is
// the steady state - not a problem. The previous IsActive
// implementation flattened Ready/Disabled to false, which made
// Status report "not loaded" and triggered the drift hint
// "settings say enabled, but the OS daemon manager has no loaded
// service" on every status check between worker runs.
//
// The test pins the contract by mocking a schtasks /Query that
// returns CSV with Status="Ready" and asserts IsRegistered
// returns true (a Ready task IS registered).
func TestIsRegistered_ReadyTaskCountsAsRegistered(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "schtasks.cmd")
	// Fake schtasks: any /Query exits 0 with a CSV row whose
	// Status column reads "Ready". Other invocations exit 0 with
	// no output.
	script := `@echo off
echo "\Semantica\sh.semantica.worker","N/A","Ready"
exit /b 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("seed fake schtasks: %v", err)
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
		t.Errorf("Ready task must report registered=true; otherwise Status would render drift between worker runs")
	}
}

// Disable on Windows must remain best-effort even if schtasks.exe
// is unavailable or fails. Mirrors the Linux Disable contract:
// users in degraded environments need to clean up state.
//
// The test seeds the XML file and settings, then runs Disable.
// schtasks may or may not exist on the test runner - either way,
// Disable should remove the XML file and clear settings without
// returning ErrUnsupportedOS or a hard failure. The schtasks
// Delete call is best-effort under windowsManager.Uninstall (see
// the `_ = deleteTask(...)` line).
func TestDisable_BestEffortRemovesXMLAndClearsSettings(t *testing.T) {
	semHome := t.TempDir()
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SEMANTICA_HOME", semHome)

	xmlPath, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(xmlPath), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(xmlPath, []byte("<Task/>"), 0o644); err != nil {
		t.Fatalf("seed task xml: %v", err)
	}
	if err := WriteSettings(UserSettings{
		Launcher: LauncherSettings{
			Enabled:           true,
			InstalledUnitPath: xmlPath,
			InstalledAt:       1,
		},
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	result, err := Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable must not error in degraded environments, got: %v", err)
	}
	if result.RemovedUnitPath != xmlPath {
		t.Errorf("RemovedUnitPath = %q, want %q", result.RemovedUnitPath, xmlPath)
	}
	if _, err := os.Stat(xmlPath); !os.IsNotExist(err) {
		t.Errorf("task XML still on disk after Disable: stat=%v", err)
	}
	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings post-Disable: %v", err)
	}
	if s.Launcher.Enabled {
		t.Errorf("Launcher.Enabled = true after Disable; settings not cleared")
	}
}
