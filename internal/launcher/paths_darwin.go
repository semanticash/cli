//go:build darwin

package launcher

import (
	"fmt"
	"os"
	"path/filepath"
)

// UnitPath returns the installed plist path for the current user.
// The OS-neutral name is preserved on platforms with non-plist
// daemon definitions (systemd unit files, Task Scheduler tasks).
func UnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LabelWorker+".plist"), nil
}

// UserDomain returns the launchctl gui/<uid> domain for the
// current user.
func UserDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// UnitTarget returns the launchctl gui/<uid>/<label> target.
func UnitTarget() string {
	return UserDomain() + "/" + LabelWorker
}
