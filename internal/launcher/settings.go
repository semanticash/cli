// Package launcher manages the optional OS-backed worker launcher.
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
//
// The canonical on-disk install-path key is "installed_unit_path".
// During the transition, the legacy "installed_plist_path" key is
// also written with the same value, and the read path accepts either
// key.
type LauncherSettings struct {
	// Enabled reports whether the launcher is enabled.
	Enabled bool `json:"enabled"`

	// InstalledUnitPath is the launcher install path written by
	// Enable. The JSON tag is "installed_unit_path"; the legacy
	// "installed_plist_path" key is handled by the dual-key
	// MarshalJSON / UnmarshalJSON below.
	InstalledUnitPath string `json:"installed_unit_path,omitempty"`

	// InstalledAt is the enable-time Unix millisecond timestamp.
	InstalledAt int64 `json:"installed_at,omitempty"`
}

// MarshalJSON writes both install-path keys with the same value while
// the legacy key is still supported.
func (s LauncherSettings) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enabled            bool   `json:"enabled"`
		InstalledUnitPath  string `json:"installed_unit_path,omitempty"`
		InstalledPlistPath string `json:"installed_plist_path,omitempty"`
		InstalledAt        int64  `json:"installed_at,omitempty"`
	}{
		Enabled:            s.Enabled,
		InstalledUnitPath:  s.InstalledUnitPath,
		InstalledPlistPath: s.InstalledUnitPath,
		InstalledAt:        s.InstalledAt,
	})
}

// UnmarshalJSON reads both install-path keys and prefers
// installed_unit_path when it is present, even if it is the empty
// string. A nil pointer means the key was absent or null, so the
// legacy installed_plist_path fallback applies.
//
// Conflicting non-empty values resolve to the canonical key. The
// next WriteSettings overwrites both keys with the canonical value,
// so the conflict cannot persist past one read/write cycle.
func (s *LauncherSettings) UnmarshalJSON(data []byte) error {
	var aux struct {
		Enabled            bool    `json:"enabled"`
		InstalledUnitPath  *string `json:"installed_unit_path,omitempty"`
		InstalledPlistPath *string `json:"installed_plist_path,omitempty"`
		InstalledAt        int64   `json:"installed_at,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.Enabled = aux.Enabled
	s.InstalledAt = aux.InstalledAt
	switch {
	case aux.InstalledUnitPath != nil:
		s.InstalledUnitPath = *aux.InstalledUnitPath
	case aux.InstalledPlistPath != nil:
		s.InstalledUnitPath = *aux.InstalledPlistPath
	}
	return nil
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
