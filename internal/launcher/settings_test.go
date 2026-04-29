package launcher

import (
	"encoding/json"
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

// --- Dual-key migration: installed_plist_path -> installed_unit_path ---

// TestSettings_ReadsLegacyPlistKey checks the legacy read path.
func TestSettings_ReadsLegacyPlistKey(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	const legacy = `{
		"launcher": {
			"enabled": true,
			"installed_plist_path": "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
			"installed_at": 1714000000000
		}
	}`
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	got, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	want := LauncherSettings{
		Enabled:            true,
		InstalledPlistPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
		InstalledAt:        1714000000000,
	}
	if got.Launcher != want {
		t.Errorf("legacy file read: got %+v, want %+v", got.Launcher, want)
	}
}

// TestSettings_ReadsBothKeysSameValue checks the dual-write file
// shape used during the migration.
func TestSettings_ReadsBothKeysSameValue(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	const dualWritten = `{
		"launcher": {
			"enabled": true,
			"installed_plist_path": "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
			"installed_unit_path": "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
			"installed_at": 1714000000000
		}
	}`
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte(dualWritten), 0o644); err != nil {
		t.Fatalf("seed dual-key file: %v", err)
	}

	got, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	want := LauncherSettings{
		Enabled:            true,
		InstalledPlistPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
		InstalledAt:        1714000000000,
	}
	if got.Launcher != want {
		t.Errorf("dual-key file read: got %+v, want %+v", got.Launcher, want)
	}
}

// TestSettings_DualWritesEmitBothKeys checks that WriteSettings emits
// both install-path keys during the migration.
func TestSettings_DualWritesEmitBothKeys(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	const path = "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist"
	if err := WriteSettings(UserSettings{
		Launcher: LauncherSettings{
			Enabled:            true,
			InstalledPlistPath: path,
			InstalledAt:        1714000000000,
		},
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(base, "settings.json"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"installed_plist_path"`) {
		t.Errorf("written file missing legacy key; got:\n%s", body)
	}
	if !strings.Contains(body, `"installed_unit_path"`) {
		t.Errorf("written file missing canonical key; got:\n%s", body)
	}
	// Re-parse rather than matching raw JSON so key order does not matter.
	var raw2 struct {
		Launcher struct {
			Plist string `json:"installed_plist_path"`
			Unit  string `json:"installed_unit_path"`
		} `json:"launcher"`
	}
	if err := json.Unmarshal(raw, &raw2); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if raw2.Launcher.Plist != path {
		t.Errorf("installed_plist_path = %q, want %q", raw2.Launcher.Plist, path)
	}
	if raw2.Launcher.Unit != path {
		t.Errorf("installed_unit_path = %q, want %q", raw2.Launcher.Unit, path)
	}
}

// TestSettings_RoundTripsLosslessly checks the write/read round trip.
func TestSettings_RoundTripsLosslessly(t *testing.T) {
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
		t.Fatalf("ReadSettings: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSettings_NewKeyWinsOnConflict checks that the new key wins when
// both install-path keys are present.
func TestSettings_NewKeyWinsOnConflict(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	const conflicting = `{
		"launcher": {
			"enabled": true,
			"installed_plist_path": "/old/path.plist",
			"installed_unit_path": "/new/path.plist",
			"installed_at": 1714000000000
		}
	}`
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte(conflicting), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if got := s.Launcher.InstalledPlistPath; got != "/new/path.plist" {
		t.Errorf("conflict tiebreak: got %q, want %q (new key must win)", got, "/new/path.plist")
	}

	// On rewrite, both keys carry the new value.
	if err := WriteSettings(s); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(base, "settings.json"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if strings.Contains(string(raw), "/old/path.plist") {
		t.Errorf("rewrite must drop the old conflicting value; got:\n%s", string(raw))
	}
}

// TestSettings_NewKeyPresentEmptyClearsLegacy checks that an explicit
// empty new key clears the legacy value.
func TestSettings_NewKeyPresentEmptyClearsLegacy(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	const handEdited = `{
		"launcher": {
			"enabled": true,
			"installed_plist_path": "/old/path.plist",
			"installed_unit_path": "",
			"installed_at": 1714000000000
		}
	}`
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte(handEdited), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if got := s.Launcher.InstalledPlistPath; got != "" {
		t.Errorf("explicit empty new key must win: got %q, want empty (legacy must NOT resurrect)", got)
	}

	// Confirm the rewrite keeps the cleared state: legacy value gone.
	if err := WriteSettings(s); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(base, "settings.json"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if strings.Contains(string(raw), "/old/path.plist") {
		t.Errorf("rewrite must drop the legacy value; got:\n%s", string(raw))
	}
}

// TestSettings_NewKeyAbsentFallsThroughToLegacy checks that absent or
// null installed_unit_path still falls back to the legacy key.
func TestSettings_NewKeyAbsentFallsThroughToLegacy(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	cases := []struct {
		name string
		body string
	}{
		{
			name: "key absent",
			body: `{"launcher": {"enabled": true, "installed_plist_path": "/legacy.plist"}}`,
		},
		{
			name: "key explicitly null",
			body: `{"launcher": {"enabled": true, "installed_plist_path": "/legacy.plist", "installed_unit_path": null}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte(c.body), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			s, err := ReadSettings()
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			if got := s.Launcher.InstalledPlistPath; got != "/legacy.plist" {
				t.Errorf("legacy fallback failed: got %q, want %q", got, "/legacy.plist")
			}
		})
	}
}

// TestSettings_EmptyInstalledPathOmitsBothKeys pins that a zero
// LauncherSettings does not emit either key. omitempty on both
// fields means a clean { "enabled": false } body, not
// { "enabled": false, "installed_plist_path": "", "installed_unit_path": "" }.
func TestSettings_EmptyInstalledPathOmitsBothKeys(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	if err := WriteSettings(UserSettings{
		Launcher: LauncherSettings{Enabled: true},
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(base, "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "installed_plist_path") {
		t.Errorf("empty install path must not emit installed_plist_path; got:\n%s", body)
	}
	if strings.Contains(body, "installed_unit_path") {
		t.Errorf("empty install path must not emit installed_unit_path; got:\n%s", body)
	}
}
