package launcher

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- identityStale -----------------------------------------------------------

func TestIdentityStale(t *testing.T) {
	home := t.TempDir()
	bin := fakeBinary(t, home)
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

// --- Enable records identity / Refresh / EnsureFreshBinary -------------------

func TestEnable_RecordsBinaryIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	home, _ := setupInstallEnv(t)
	writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	if _, err := Enable(context.Background(), bin); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	recorded, ok := s.Launcher.RecordedIdentity()
	if !ok {
		t.Fatal("Enable did not record a binary identity")
	}
	current, _ := StatBinaryIdentity(bin)
	if recorded != current {
		t.Errorf("recorded identity %+v != current %+v", recorded, current)
	}
	if stale, reason := identityStale(s.Launcher); stale {
		t.Errorf("freshly enabled launcher reads stale: %s", reason)
	}
}

func TestRefresh_NoopWhenDisabled(t *testing.T) {
	setupInstallEnv(t)
	res, err := Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Enabled {
		t.Error("Refresh must be a no-op when the launcher is disabled")
	}
}

func TestEnsureFreshBinary_SelfHealsAfterReplacement(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	home, _ := setupInstallEnv(t)
	writeStatefulFakeLaunchctl(t)
	bin := fakeBinary(t, home)

	if _, err := Enable(context.Background(), bin); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Fresh binary: no refresh.
	refreshed, err := EnsureFreshBinary(context.Background())
	if err != nil {
		t.Fatalf("EnsureFreshBinary (fresh): %v", err)
	}
	if refreshed {
		t.Error("fresh binary must not trigger a refresh")
	}

	// Simulate `make install`: replace the binary in place with new
	// content and mtime.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n# v2\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("replace binary: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	refreshed, err = EnsureFreshBinary(context.Background())
	if err != nil {
		t.Fatalf("EnsureFreshBinary (stale): %v", err)
	}
	if !refreshed {
		t.Fatal("replaced binary must trigger a refresh")
	}

	// The refresh re-recorded the new identity: a second check is a no-op.
	refreshed, err = EnsureFreshBinary(context.Background())
	if err != nil {
		t.Fatalf("EnsureFreshBinary (post-refresh): %v", err)
	}
	if refreshed {
		t.Error("identity must be re-recorded by refresh; second check refreshed again")
	}
}

// TestRefresh_BindsToCurrentExecutableNotRecordedPath pins the
// install-migration case: a launcher enabled against an old install
// location must re-bind to the currently executing binary on refresh, not
// re-bootstrap the old path and stamp a fresh identity onto it (which
// would make status look healthy while launchd runs the wrong build).
func TestRefresh_BindsToCurrentExecutableNotRecordedPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	home, _ := setupInstallEnv(t)
	writeStatefulFakeLaunchctl(t)
	oldBin := fakeBinary(t, home)

	if _, err := Enable(context.Background(), oldBin); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	res, err := Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if res.BinaryPath != exe {
		t.Errorf("refresh bound to %q, want current executable %q", res.BinaryPath, exe)
	}
	if res.BinaryPath == oldBin {
		t.Error("refresh must not re-bind the recorded (old) binary path")
	}

	s, err := ReadSettings()
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if s.Launcher.InstalledBinaryPath != exe {
		t.Errorf("recorded identity path = %q, want %q", s.Launcher.InstalledBinaryPath, exe)
	}
}
