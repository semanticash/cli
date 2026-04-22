// Package launcher manages the optional macOS launchd worker.
package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/platform"
)

// UserSettings is the user-level settings file at
// $HOME/.semantica/settings.json.
type UserSettings struct {
	Launcher LauncherSettings `json:"launcher,omitempty"`
}

// LauncherSettings records the launcher's installed state.
type LauncherSettings struct {
	// Enabled reports whether the launcher is enabled.
	Enabled bool `json:"enabled"`

	// InstalledPlistPath is the plist written by enable.
	InstalledPlistPath string `json:"installed_plist_path,omitempty"`

	// InstalledAt is the enable-time Unix millisecond timestamp.
	InstalledAt int64 `json:"installed_at,omitempty"`
}

// SettingsPath returns the launcher settings path. It honors
// SEMANTICA_HOME via broker.GlobalBase.
func SettingsPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "settings.json"), nil
}

// ReadSettings loads the user-level settings file. A missing file
// returns the zero value. A malformed file returns an error.
func ReadSettings() (UserSettings, error) {
	path, err := SettingsPath()
	if err != nil {
		return UserSettings{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UserSettings{}, nil
		}
		return UserSettings{}, err
	}
	var s UserSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return UserSettings{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// WriteSettings atomically writes the user-level settings file.
func WriteSettings(s UserSettings) error {
	path, err := SettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create user settings dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := platform.SafeRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// IsEnabled reports whether the launcher is enabled. Read errors
// fall back to false so hook-side callers do not break commits.
func IsEnabled() bool {
	s, err := ReadSettings()
	if err != nil {
		return false
	}
	return s.Launcher.Enabled
}
