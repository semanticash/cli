//go:build unix

package platform

import (
	"os"
	"syscall"
)

// LockFile acquires an exclusive lock on the entire file.
func LockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// UnlockFile releases the lock on the file.
func UnlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
