package platform

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// SafeRename moves src to dst, replacing dst if it exists.
// On Windows, removes dst first (os.Rename cannot overwrite) and
// retries only on transient sharing-violation errors from antivirus
// or search indexing. Non-transient errors fail immediately.
// This is NOT atomic on Windows.
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

// isTransientError checks for Windows sharing-violation and lock-violation
// errors that indicate a transient file handle hold (antivirus, indexing).
func isTransientError(err error) bool {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		// ERROR_SHARING_VIOLATION (32) and ERROR_LOCK_VIOLATION (33).
		return errno == 32 || errno == 33
	}
	return false
}
