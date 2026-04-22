package launcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
)

// workerLogBasename is the filename used for the launchd agent's
// captured stdout and stderr. Kept as a constant so tests and
// diagnostic tooling can form the full path without guessing.
const workerLogBasename = "worker-launcher.log"

// PlistPath returns the absolute path where the worker plist
// should be installed for the current user. The path honors the
// caller's HOME env var (via os.UserHomeDir), which makes it
// safe for tests that stand up an isolated home directory via
// t.Setenv("HOME", ...).
//
// The directory is not created here; enable flows that need to
// write the plist create ~/Library/LaunchAgents before the
// atomic rename.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LabelWorker+".plist"), nil
}

// WorkerLogPath returns the absolute path where the launchd
// agent's stdout and stderr are captured. It lives under the
// shared semantica global base (honors SEMANTICA_HOME for tests)
// so a user who wipes the global base also wipes the launcher
// log, and so both files share a single life cycle.
func WorkerLogPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, workerLogBasename), nil
}

// UserDomain returns the launchctl bootstrap context for the
// current user, in the form "gui/<uid>". Used as the first
// argument to launchctl bootstrap when installing the plist.
func UserDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// DomainTarget returns the launchctl service target for the
// worker agent, in the form "gui/<uid>/<label>". This is what
// launchctl bootout, kickstart, and print take when they need
// to identify the service.
func DomainTarget() string {
	return UserDomain() + "/" + LabelWorker
}
