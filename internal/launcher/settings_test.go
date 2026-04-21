package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsPath_HonorsSemanticaHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	got, err := SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	want := filepath.Join(base, "settings.json")
	if got != want {
		t.Errorf("SettingsPath = %q, want %q", got, want)
	}
}

func TestReadSettings_MissingFileReturnsZero(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings on missing file: %v", err)
	}
	if s.Launcher.Enabled {
		t.Errorf("expected zero-value settings, got Launcher.Enabled=true")
	}
}

func TestReadSettings_MalformedFileReturnsError(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed malformed file: %v", err)
	}

	_, err := ReadSettings()
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected wrapped parse error, got %v", err)
	}
}

func TestWriteSettings_RoundTripsLauncherSection(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	want := UserSettings{
		Launcher: LauncherSettings{
			Enabled:            true,
			InstalledPlistPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
			InstalledAt:        1714000000000,
		},
	}
	if err := WriteSettings(want); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	got, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings after write: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestWriteSettings_CreatesParentDirIfMissing(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nested", "deeper")
	t.Setenv("SEMANTICA_HOME", base)

	if err := WriteSettings(UserSettings{Launcher: LauncherSettings{Enabled: true}}); err != nil {
		t.Fatalf("WriteSettings on missing parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "settings.json")); err != nil {
		t.Errorf("expected file at %s, got stat error %v", base, err)
	}
}

func TestWriteSettings_AtomicLeavesNoTempOnSuccess(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	if err := WriteSettings(UserSettings{Launcher: LauncherSettings{Enabled: true}}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "settings.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file lingered after successful write, stat=%v", err)
	}
}

func TestIsEnabled_FalseOnMissingFile(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if IsEnabled() {
		t.Error("IsEnabled = true when settings file does not exist")
	}
}

func TestIsEnabled_FalseOnMalformedFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if IsEnabled() {
		t.Error("IsEnabled = true on malformed settings; must treat as not enabled")
	}
}

func TestIsEnabled_TrueWhenLauncherEnabled(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := WriteSettings(UserSettings{Launcher: LauncherSettings{Enabled: true}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !IsEnabled() {
		t.Error("IsEnabled = false after writing Enabled=true")
	}
}

func TestIsEnabled_FalseWhenLauncherExplicitlyDisabled(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := WriteSettings(UserSettings{Launcher: LauncherSettings{Enabled: false}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if IsEnabled() {
		t.Error("IsEnabled = true when Enabled=false was persisted")
	}
}
