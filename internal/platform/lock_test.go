package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockFile_ExclusiveLock(t *testing.T) {
	f := createTempLockFile(t)

	if err := LockFile(f); err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	if err := UnlockFile(f); err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
}

func TestLockFile_LockUnlockRelock(t *testing.T) {
	f := createTempLockFile(t)

	if err := LockFile(f); err != nil {
		t.Fatalf("first LockFile: %v", err)
	}
	if err := UnlockFile(f); err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
	if err := LockFile(f); err != nil {
		t.Fatalf("second LockFile: %v", err)
	}
	if err := UnlockFile(f); err != nil {
		t.Fatalf("second UnlockFile: %v", err)
	}
}

func TestLockFile_CrossProcessContention(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: cross-process locking test")
	}
	t.Skip("TODO: implement cross-process locking test in Phase 1")
}

func createTempLockFile(t *testing.T) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lockfile")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create temp lock file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
