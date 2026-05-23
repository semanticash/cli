package platform

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// SafeRename moves src to dst, replacing dst if it exists.
// On Windows, it removes dst first because os.Rename cannot overwrite
// an existing file. It retries transient sharing, lock, and access
// errors caused by concurrent readers or writers.
// This is not atomic on Windows.
func SafeRename(src, dst string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dst)
	}
	_ = os.Remove(dst)
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !isTransientError(err) {
		return err
	}
	for i := 1; i <= 3; i++ {
		time.Sleep(time.Duration(i*50) * time.Millisecond)
		_ = os.Remove(dst)
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		if !isTransientError(err) {
			return err
		}
	}
	return err
}

// isTransientError reports whether SafeRename should retry err.
//
//   - ERROR_SHARING_VIOLATION (32): another process has the file open
//     with an incompatible share mode.
//   - ERROR_LOCK_VIOLATION (33): another process holds a conflicting
//     file lock.
//   - ERROR_ACCESS_DENIED (5): a concurrent writer may have recreated
//     dst between this writer's remove and rename calls.
//
// Access denied can also mean a real permission error, but the bounded
// retry keeps that cost small.
func isTransientError(err error) bool {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno == 5 || errno == 32 || errno == 33
	}
	return false
}
