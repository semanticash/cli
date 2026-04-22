package launcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
)

// workerLogBasename is the launchd worker log filename.
const workerLogBasename = "worker-launcher.log"

// PlistPath returns the installed plist path for the current user.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LabelWorker+".plist"), nil
}

// WorkerLogPath returns the launchd worker log path.
func WorkerLogPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, workerLogBasename), nil
}

// UserDomain returns gui/<uid>.
func UserDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// DomainTarget returns gui/<uid>/<label>.
func DomainTarget() string {
	return UserDomain() + "/" + LabelWorker
}
