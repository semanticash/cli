package hooks

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// These tests lock the CaptureDirWritable success and failure contract.

func TestCaptureDirWritable_SucceedsOnNormalHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)

	if err := CaptureDirWritable(); err != nil {
		t.Fatalf("probe failed on writable dir: %v", err)
	}

	// The probe file must be cleaned up; leftover probe files
	// would pile up across reconcile invocations.
	captureBase := filepath.Join(dir, "capture")
	probe := filepath.Join(captureBase, ".writable-probe")
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Errorf("expected probe file to be removed, stat=%v", err)
	}
}

func TestCaptureDirWritable_ReportsPermissionErr(t *testing.T) {
	if runtime.GOOS == "windows" {
		// POSIX chmod-based denial is not portable to Windows.
		t.Skip("POSIX-mode permission denial cannot be simulated on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod-based permission checks")
	}

	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	captureBase := filepath.Join(home, "capture")
	if err := os.MkdirAll(captureBase, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Make the capture directory unwritable. The probe should match
	// fs.ErrPermission.
	if err := os.Chmod(captureBase, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(captureBase, 0o755) })

	err := CaptureDirWritable()
	if err == nil {
		t.Fatal("expected error on unwritable capture dir, got nil")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("expected fs.ErrPermission, got %v", err)
	}
}
