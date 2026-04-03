package util

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type PlaybookAutomation struct {
	Enabled bool `json:"enabled"`
}

type Automations struct {
	Playbook PlaybookAutomation `json:"playbook"`
}

type Settings struct {
	Enabled         bool         `json:"enabled"`
	Version         int          `json:"version"`
	Providers       []string     `json:"providers,omitempty"`
	Trailers        *bool        `json:"trailers,omitempty"`
	Automations     *Automations `json:"automations,omitempty"`
	Connected       bool         `json:"connected"`
	ConnectedRepoID string       `json:"connected_repo_id,omitempty"`
}

func SettingsPath(semDir string) string {
	return filepath.Join(semDir, "settings.json")
}

// ReadSettings reads the settings file from the given .semantica directory.
// Returns zero-value Settings (Enabled: false) if the file is missing.
// Returns an error if the file exists but cannot be parsed, so callers
// can distinguish "not configured" from "settings format has changed."
func ReadSettings(semDir string) (Settings, error) {
	data, err := os.ReadFile(SettingsPath(semDir))
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{}, nil
		}
		return Settings{}, err
	}

	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("parse settings.json: %w (binary may be outdated)", err)
	}
	return s, nil
}

// WriteSettings atomically writes the settings file via tmp+rename.
// It also maintains a .semantica/enabled marker file used by Git hooks
// for a reliable enabled check without parsing JSON in shell.
func WriteSettings(semDir string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := SettingsPath(semDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	// Sync the marker file with the enabled state.
	marker := filepath.Join(semDir, "enabled")
	if s.Enabled {
		// Touch the marker file.
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return err
		}
	} else {
		// Remove the marker; ignore if already absent.
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// IsEnabled checks whether Semantica is enabled by looking for the marker
// file (.semantica/enabled) rather than parsing settings.json.
func IsEnabled(semDir string) bool {
	_, err := os.Stat(filepath.Join(semDir, "enabled"))
	return err == nil
}

// IsEnabledAt checks whether Semantica is enabled for a repo at the given path.
func IsEnabledAt(repoPath string) bool {
	return IsEnabled(filepath.Join(repoPath, ".semantica"))
}

// IsConnected returns true if the repo is connected to Semantica.
func IsConnected(semDir string) bool {
	s, err := ReadSettings(semDir)
	if err != nil {
		return false
	}
	return s.Connected
}

// TrailersEnabled returns whether attribution and diagnostics trailers are on.
// Defaults to true when the key is absent or settings cannot be read.
func TrailersEnabled(semDir string) bool {
	s, err := ReadSettings(semDir)
	if err != nil || s.Trailers == nil {
		return true
	}
	return *s.Trailers
}

// IsPlaybookEnabled returns true if the auto-playbook automation is enabled.
func IsPlaybookEnabled(semDir string) bool {
	s, err := ReadSettings(semDir)
	if err != nil {
		return false
	}
	if s.Automations == nil {
		return false
	}
	return s.Automations.Playbook.Enabled
}
