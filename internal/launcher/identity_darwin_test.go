//go:build darwin

package launcher

import (
	"context"
	"os"
	"testing"
	"time"
)

// Identity tests that need the darwin launchctl harness from
// install_test.go (setupInstallEnv, fakeBinary, writeStatefulFakeLaunchctl).

func TestEnable_RecordsBinaryIdentity(t *testing.T) {
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

func TestEnsureFreshBinary_SelfHealsAfterReplacement(t *testing.T) {
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

	// Simulate a local reinstall: replace the binary in place with new
	// content and mtime, which can leave launchd refusing spawns until
	// the service is re-registered.
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
