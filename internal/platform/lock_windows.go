//go:build windows

package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// LockFile acquires an exclusive lock on the entire file.
// Uses max uint32 for both length fields to cover the whole file,
// matching the whole-file semantics of Unix flock(LOCK_EX).
func LockFile(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		&windows.Overlapped{},
	)
}

// UnlockFile releases the lock on the file.
func UnlockFile(f *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		0xFFFFFFFF,
		0xFFFFFFFF,
		&windows.Overlapped{},
	)
}
