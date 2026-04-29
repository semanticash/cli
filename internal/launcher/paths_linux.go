//go:build linux

package launcher

import (
	"fmt"
	"os"
	"path/filepath"
)

// UnitPath returns the systemd user unit path for the worker.
// Honors XDG_CONFIG_HOME via os.UserConfigDir; falls back to
// $HOME/.config when XDG_CONFIG_HOME is unset.
func UnitPath() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(cfgDir, "systemd", "user", LabelWorker+".service"), nil
}

// UserDomain returns the empty string on Linux. The systemd user
// instance has no analog to launchctl's gui/<uid> tuple; the
// systemctl --user invocation is the access point.
func UserDomain() string {
	return ""
}

// UnitTarget returns the systemd unit name. systemctl --user
// accepts this string as the unit argument.
func UnitTarget() string {
	return LabelWorker + ".service"
}
