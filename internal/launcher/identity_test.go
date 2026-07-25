package launcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Platform-neutral identity tests. The darwin-only launchctl harness
// (setupInstallEnv, fakeBinary, writeStatefulFakeLaunchctl) lives in
// darwin-tagged files; tests that need it are in identity_darwin_test.go.

// writeExecutableFile creates an executable file usable as a stand-in
// binary for identity checks on any platform.
func writeExecutableFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantica-test-bin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

// isolateSettingsEnv points HOME and SEMANTICA_HOME at temp dirs so
// settings reads/writes never touch the real user state.
func isolateSettingsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SEMANTICA_HOME", t.TempDir())
}

// --- identityStale -----------------------------------------------------------

func TestIdentityStale(t *testing.T) {
	bin := writeExecutableFile(t)
	current, err := StatBinaryIdentity(bin)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	fresh := LauncherSettings{
		Enabled:              true,
		InstalledBinaryPath:  current.Path,
		InstalledBinarySize:  current.Size,
		InstalledBinaryModMS: current.ModMS,
	}
	if stale, reason := identityStale(fresh); stale {
		t.Errorf("matching identity reported stale: %s", reason)
	}

	// Legacy settings (no recorded identity) are stale by definition so
	// one refresh migrates them.
	if stale, _ := identityStale(LauncherSettings{Enabled: true}); !stale {
		t.Error("missing identity record must read as stale")
	}

	// In-place replacement: contents (size) change.
	replaced := fresh
	replaced.InstalledBinarySize = current.Size + 1
	if stale, _ := identityStale(replaced); !stale {
		t.Error("size drift must read as stale")
	}

	// In-place rebuild with same size: mtime changes.
	rebuilt := fresh
	rebuilt.InstalledBinaryModMS = current.ModMS - 1000
	if stale, _ := identityStale(rebuilt); !stale {
		t.Error("mtime drift must read as stale")
	}

	// Registered binary gone entirely.
	missing := fresh
	missing.InstalledBinaryPath = bin + ".gone"
	if stale, _ := identityStale(missing); !stale {
		t.Error("missing binary must read as stale")
	}
}

// --- settings roundtrip ------------------------------------------------------

func TestLauncherSettings_IdentityRoundtrip(t *testing.T) {
	in := LauncherSettings{
		Enabled:              true,
		InstalledUnitPath:    "/tmp/unit.plist",
		InstalledAt:          1_700_000_000_000,
		InstalledBinaryPath:  "/usr/local/bin/semantica",
		InstalledBinarySize:  33_674_034,
		InstalledBinaryModMS: 1_753_447_140_000,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Dual-key legacy contract still holds with the new fields present.
	if !strings.Contains(string(data), `"installed_plist_path"`) {
		t.Errorf("legacy key missing from marshal: %s", data)
	}
	var out LauncherSettings
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

// --- Refresh (platform-neutral behavior) -------------------------------------

func TestRefresh_NoopWhenDisabled(t *testing.T) {
	isolateSettingsEnv(t)
	res, err := Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Enabled {
		t.Error("Refresh must be a no-op when the launcher is disabled")
	}
}
