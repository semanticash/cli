package platform

import (
	"bufio"
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
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "contended.lock")
	dataPath := filepath.Join(dir, "data.txt")

	cmd := startHelper(t, "lockholder",
		"SEMANTICA_HELPER_LOCK_PATH="+lockPath,
		"SEMANTICA_HELPER_DATA_PATH="+dataPath,
		"SEMANTICA_HELPER_HOLD_MS=300",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	acquired := false
	for scanner.Scan() {
		if scanner.Text() == "LOCKED" {
			acquired = true
			break
		}
	}
	if !acquired {
		_ = cmd.Wait()
		t.Fatal("lock holder never signaled LOCKED")
	}

	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := LockFile(f); err != nil {
		t.Fatalf("LockFile under contention: %v", err)
	}
	defer func() { _ = UnlockFile(f) }()

	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("lock acquired before holder released (data file missing): %v", err)
	}
	if string(got) != "written-under-lock" {
		t.Errorf("data file = %q, want written-under-lock", got)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock holder exited with error: %v", err)
	}
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
