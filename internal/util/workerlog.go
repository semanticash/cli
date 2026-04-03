package util

import (
	"os"
	"path/filepath"
)

// OpenWorkerLog opens .semantica/worker.log in append mode.
// The caller is responsible for closing the returned file.
func OpenWorkerLog(semDir string) (*os.File, error) {
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(
		filepath.Join(semDir, "worker.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0o644,
	)
}
