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

func TestTryLockFile_ContendedAndFree(t *testing.T) {
	f := createTempLockFile(t)
	if err := LockFile(f); err != nil {
		t.Fatalf("LockFile: %v", err)
	}

	// A second descriptor for the same file must not acquire.
	f2, err := os.OpenFile(f.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	got, err := TryLockFile(f2)
	if err != nil {
		t.Fatalf("TryLockFile contended: %v", err)
	}
	if got {
		t.Fatal("TryLockFile acquired a lock another descriptor holds")
	}

	if err := UnlockFile(f); err != nil {
		t.Fatalf("UnlockFile: %v", err)
	}
	got, err = TryLockFile(f2)
	if err != nil {
		t.Fatalf("TryLockFile free: %v", err)
	}
	if !got {
		t.Fatal("TryLockFile failed to acquire a free lock")
	}
	if err := UnlockFile(f2); err != nil {
		t.Fatalf("UnlockFile second descriptor: %v", err)
	}
}

func TestLockFile_ContentReadableWhileHeld(t *testing.T) {
	f := createTempLockFile(t)
	const meta = `{"pid":1234}`
	if _, err := f.WriteAt([]byte(meta), 0); err != nil {
		t.Fatal(err)
	}
	if err := LockFile(f); err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	defer func() { _ = UnlockFile(f) }()

	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("metadata unreadable while lock held: %v", err)
	}
	if string(got) != meta {
		t.Errorf("metadata = %q, want %q", got, meta)
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
