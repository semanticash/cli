package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/platform"
)

type PlaybookAutomation struct {
	Enabled bool `json:"enabled"`
}

type ImplementationSummaryAutomation struct {
	Enabled bool `json:"enabled"`
}

type Automations struct {
	Playbook              PlaybookAutomation              `json:"playbook"`
	ImplementationSummary ImplementationSummaryAutomation `json:"implementation_summary"`
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
	if err := platform.SafeRename(tmp, path); err != nil {
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

// IsImplementationSummaryEnabled returns true if the auto-implementation-summary automation is enabled.
// For existing installations that predate this setting, the field is absent in
// settings.json and defaults to true (matching the semantica enable default).
// On first read, the setting is backfilled so future reads are explicit.
func IsImplementationSummaryEnabled(semDir string) bool {
	s, err := ReadSettings(semDir)
	if err != nil {
		return false
	}
	if s.Automations == nil {
		// Very old install with no automations block at all.
		// Backfill with both defaults matching what semantica enable writes,
		// so we don't accidentally disable playbook as a side effect.
		s.Automations = &Automations{
			Playbook:              PlaybookAutomation{Enabled: true},
			ImplementationSummary: ImplementationSummaryAutomation{Enabled: true},
		}
		if err := WriteSettings(semDir, s); err != nil {
			log.Printf("semantica: backfill implementation_summary setting: %v", err)
		}
		return true
	}

	// Check if the key is actually present in the raw JSON.
	// If absent, backfill it as enabled (default for new installs).
	raw, readErr := os.ReadFile(SettingsPath(semDir))
	if readErr == nil && !jsonKeyExists(raw, "implementation_summary") {
		s.Automations.ImplementationSummary.Enabled = true
		if err := WriteSettings(semDir, s); err != nil {
			log.Printf("semantica: backfill implementation_summary setting: %v", err)
		}
		return true
	}

	return s.Automations.ImplementationSummary.Enabled
}

// jsonKeyExists checks whether a key appears anywhere in the raw JSON bytes.
// This is a simple substring check - sufficient for detecting the presence
// of a settings key without full JSON path traversal.
func jsonKeyExists(data []byte, key string) bool {
	needle := fmt.Sprintf(`"%s"`, key)
	return bytes.Contains(data, []byte(needle))
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
