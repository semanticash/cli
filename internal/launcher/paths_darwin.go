//go:build darwin

package launcher

import (
	"fmt"
	"os"
	"path/filepath"
)

// PlistPath returns the installed plist path for the current user.
func PlistPath() (string, error) {
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

// DomainTarget returns the launchctl gui/<uid>/<label> target.
func DomainTarget() string {
	return UserDomain() + "/" + LabelWorker
}
