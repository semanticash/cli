package launcher

import (
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
)

// workerLogBasename is the launcher worker log filename. Cross-
// platform; the launchd plist, the systemd unit, and the Task
// Scheduler task all write to the same path under broker.GlobalBase.
const workerLogBasename = "worker-launcher.log"

// WorkerLogPath returns the launcher worker log path. Cross-platform.
func WorkerLogPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, workerLogBasename), nil
}
