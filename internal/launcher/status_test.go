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

func TestStatus_EmptySettingsReportsNotEnabled(t *testing.T) {
	setupInstallEnv(t)
	// No settings file, no plist, no launchctl.

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.SettingsEnabled {
		t.Errorf("SettingsEnabled = true on empty state")
	}
	if s.InstalledPlistPath != "" {
		t.Errorf("InstalledPlistPath = %q on empty state", s.InstalledPlistPath)
	}
	if s.InstalledAt != 0 {
		t.Errorf("InstalledAt = %d on empty state", s.InstalledAt)
	}
	if s.ExpectedPlistPath == "" {
		t.Errorf("ExpectedPlistPath must always be populated")
	}
	if s.PlistOnDisk {
		t.Errorf("PlistOnDisk = true when no file exists")
	}
	if s.DomainTarget == "" {
		t.Errorf("DomainTarget must always be populated")
	}
	if s.LogPath == "" {
		t.Errorf("LogPath must always be populated")
	}
}

func TestStatus_ReflectsEnabledSettings(t *testing.T) {
	setupInstallEnv(t)

	// Seed settings as if Enable had run.
	recorded := "/some/recorded/plist/path.plist"
	want := UserSettings{
		Launcher: LauncherSettings{
			Enabled:            true,
			InstalledPlistPath: recorded,
			InstalledAt:        1714000000000,
		},
	}
	if err := WriteSettings(want); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !s.SettingsEnabled {
		t.Errorf("SettingsEnabled = false after seeding enabled settings")
	}
	if s.InstalledPlistPath != recorded {
		t.Errorf("InstalledPlistPath = %q, want %q", s.InstalledPlistPath, recorded)
	}
	if s.InstalledAt != 1714000000000 {
		t.Errorf("InstalledAt = %d, want 1714000000000", s.InstalledAt)
	}
}

// PlistOnDisk should track whether the plist file exists.
func TestStatus_PlistOnDiskReflectsFilesystem(t *testing.T) {
	home, _ := setupInstallEnv(t)

	// No recorded path means Status falls back to ExpectedPlistPath.
	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.PlistOnDisk {
		t.Errorf("PlistOnDisk = true before creating file")
	}

	// Create the expected plist file.
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.ExpectedPlistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	s2, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status after seeding: %v", err)
	}
	if !s2.PlistOnDisk {
		t.Errorf("PlistOnDisk = false even though file exists at %s", s2.ExpectedPlistPath)
	}
}

// A zero-exit launchctl print should report "loaded".
func TestStatus_LoadedInLaunchdWhenLaunchctlReportsSuccess(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl paths are macOS-only")
	}
	setupInstallEnv(t)
	writeFakeLaunchctl(t, 0, "")

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !s.LoadedInLaunchd {
		t.Errorf("LoadedInLaunchd = false when launchctl exits 0")
	}
	if s.LaunchdState != "loaded" {
		t.Errorf("LaunchdState = %q, want 'loaded'", s.LaunchdState)
	}
}

func TestStatus_NotLoadedWhenLaunchctlReportsServiceNotFound(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	writeFakeLaunchctl(t, 113, "Could not find service")

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.LoadedInLaunchd {
		t.Errorf("LoadedInLaunchd = true when launchctl reports not-found")
	}
	if s.LaunchdState != "not loaded" {
		t.Errorf("LaunchdState = %q, want 'not loaded'", s.LaunchdState)
	}
}

// Settings and launchd can disagree; Status should surface both.
func TestStatus_SurfacesDriftBetweenSettingsAndLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	writeFakeLaunchctl(t, 113, "Could not find service")

	if err := WriteSettings(UserSettings{
		Launcher: LauncherSettings{
			Enabled:     true,
			InstalledAt: 1,
		},
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !s.SettingsEnabled {
		t.Errorf("SettingsEnabled = false after seeding enabled")
	}
	if s.LoadedInLaunchd {
		t.Errorf("LoadedInLaunchd = true when launchctl says not-found")
	}
	if s.LaunchdState != "not loaded" {
		t.Errorf("LaunchdState = %q, want 'not loaded'", s.LaunchdState)
	}
}

// Unexpected launchctl failures should surface as an error string.
func TestStatus_UnexpectedLaunchctlErrorBecomesErrorState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	setupInstallEnv(t)
	writeFakeLaunchctl(t, 9, "Unrecognized target specifier")

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.LoadedInLaunchd {
		t.Errorf("LoadedInLaunchd = true on unexpected launchctl error")
	}
	if !strings.HasPrefix(s.LaunchdState, "error:") {
		t.Errorf("LaunchdState = %q, want 'error:' prefix", s.LaunchdState)
	}
}

// A malformed settings file should populate SettingsError.
func TestStatus_CorruptSettingsSurfacesAsSettingsError(t *testing.T) {
	_, semHome := setupInstallEnv(t)

	// Seed a malformed settings file at the canonical path.
	path := filepath.Join(semHome, "settings.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if s.SettingsEnabled {
		t.Errorf("corrupt settings must not flatten to SettingsEnabled=true, got %+v", s)
	}
	if s.SettingsError == "" {
		t.Fatal("expected SettingsError to be populated when settings file is malformed")
	}
	if !strings.Contains(s.SettingsError, "parse") {
		t.Errorf("expected SettingsError to mention parse failure, got %q", s.SettingsError)
	}
}

// A missing settings file should not populate SettingsError.
func TestStatus_MissingSettingsDoesNotPopulateError(t *testing.T) {
	setupInstallEnv(t)
	// No settings file written.

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.SettingsError != "" {
		t.Errorf("missing settings must not set SettingsError, got %q", s.SettingsError)
	}
}

