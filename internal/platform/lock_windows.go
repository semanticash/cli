//go:build windows

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows range locks are mandatory. Lock a byte outside the metadata
// region so other processes can read diagnostics while lockers still
// contend on the same byte.
const (
	lockSentinelOffset = 0xFFFFFFFE
	lockSentinelLen    = 1
)

func lockOverlapped() *windows.Overlapped {
	return &windows.Overlapped{Offset: lockSentinelOffset}
}

// LockFile acquires the file's exclusive lock, blocking until free.
func LockFile(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		lockSentinelLen,
		0,
		lockOverlapped(),
	)
}

// TryLockFile attempts to acquire an exclusive lock without blocking.
// Returns false when another holder has the lock.
func TryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockSentinelLen,
		0,
		lockOverlapped(),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// UnlockFile releases the lock on the file.
func UnlockFile(f *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		lockSentinelLen,
		0,
		lockOverlapped(),
	)
}
