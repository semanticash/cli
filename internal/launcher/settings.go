// Package launcher manages the optional macOS launchd user agent that
// runs semantica's post-commit worker outside restricted process
// lineages. The agent is user-opt-in via the `semantica launcher
// enable` and `semantica launcher disable` commands. Nothing in the
// rest of the codebase reads launcher state until a user explicitly
// opts in, so the existence of this package has no runtime effect
// on installations that do not use it.
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

// UserSettings is the top-level shape of the user-level configuration
// file at $HOME/.semantica/settings.json (or
// $SEMANTICA_HOME/settings.json when that override is set for
// tests). The file is distinct from the per-repository
// .semantica/settings.json inside a working tree; it holds settings
// that apply to the user account rather than to any one repository.
type UserSettings struct {
	Launcher LauncherSettings `json:"launcher,omitempty"`
}

// LauncherSettings records whether the opt-in launchd user agent is
// currently installed and where on disk the plist was written. The
// plist path is preserved so the disable command can remove the
// exact file that was installed, which matters when the binary has
// been upgraded between enable and disable and the default plist
// path has moved.
type LauncherSettings struct {
	// Enabled is true when the user has successfully run
	// `semantica launcher enable` and launchd has bootstrapped the
	// agent. Callers that read this field and see false should
	// treat the launcher as absent: no kickstart, no marker
	// writes, no assumption of re-parented execution.
	Enabled bool `json:"enabled"`

	// InstalledPlistPath is the absolute path of the launchd plist
	// file on disk. Empty when Enabled is false. Kept for parity
	// with launchd's bootout call, which identifies a service by
	// its label but benefits from also knowing the source file for
	// cleanup.
	InstalledPlistPath string `json:"installed_plist_path,omitempty"`

	// InstalledAt is the enable-time timestamp in Unix milliseconds.
	// Purely informational; consumed by diagnostic commands and by
	// the user inspecting the file.
	InstalledAt int64 `json:"installed_at,omitempty"`
}

// SettingsPath returns the absolute path of the user-level settings
// file. It honors the SEMANTICA_HOME environment variable via
// broker.GlobalBase, which the rest of the codebase uses for test
// isolation and for alternative install layouts.
func SettingsPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "settings.json"), nil
}

// ReadSettings loads the user-level settings file and returns the
// parsed value. A missing file is treated as an empty settings
// object rather than an error, so callers that only care about one
// section (for example, the launcher gate) can work off the
// zero-value when the user has never written the file.
//
// A file that exists but cannot be parsed is a hard error. Silently
// treating a corrupt settings file as "defaults" would mask a real
// problem and could disable features the user believed they had
// enabled.
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

// WriteSettings atomically writes the user-level settings file. It
// creates the parent directory if needed and uses a temp-file
// rename so partial writes never leave a half-parsed file in place.
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

// IsEnabled reports whether the launcher is currently opted in.
// Any read or parse error is treated as "not enabled" because the
// post-commit hook's expected behavior on an unreadable file is to
// fall through to the legacy spawn path; surfacing an error there
// would break the commit.
func IsEnabled() bool {
	s, err := ReadSettings()
	if err != nil {
		return false
	}
	return s.Launcher.Enabled
}
