package util

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/platform"
)

// PlaybookAutomation configures playbook generation while preserving unknown fields.
type PlaybookAutomation struct {
	Enabled bool
	extra   map[string]json.RawMessage
}

func (p *PlaybookAutomation) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["enabled"]; ok {
		if err := json.Unmarshal(v, &p.Enabled); err != nil {
			return err
		}
		delete(raw, "enabled")
	}
	p.extra = raw
	return nil
}

func (p PlaybookAutomation) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(p.extra)+1)
	for k, v := range p.extra {
		out[k] = v
	}
	eb, err := json.Marshal(p.Enabled)
	if err != nil {
		return nil, err
	}
	out["enabled"] = eb
	return json.Marshal(out)
}

// Automations configures repository automation while preserving unknown fields.
type Automations struct {
	Playbook PlaybookAutomation
	extra    map[string]json.RawMessage
}

func (a *Automations) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["playbook"]; ok {
		if err := json.Unmarshal(v, &a.Playbook); err != nil {
			return err
		}
		delete(raw, "playbook")
	}
	a.extra = raw
	return nil
}

func (a Automations) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(a.extra)+1)
	for k, v := range a.extra {
		out[k] = v
	}
	pb, err := json.Marshal(a.Playbook)
	if err != nil {
		return nil, err
	}
	out["playbook"] = pb
	return json.Marshal(out)
}

// Settings is the repository configuration stored in settings.json.
// Unknown fields are preserved when the file is rewritten.
type Settings struct {
	Enabled         bool
	Version         int
	Providers       []string
	Trailers        *bool
	Automations     *Automations
	Connected       bool
	ConnectedRepoID string
	// AttributionV2 controls tool-delta scoring. Nil uses the default (on).
	AttributionV2 *bool
	// WorkspaceFreeze controls optional pre-commit observations. Nil disables them.
	WorkspaceFreeze *bool
	extra           map[string]json.RawMessage
}

// settingsKnown mirrors Settings' known fields for JSON decoding.
type settingsKnown struct {
	Enabled         bool         `json:"enabled"`
	Version         int          `json:"version"`
	Providers       []string     `json:"providers,omitempty"`
	Trailers        *bool        `json:"trailers,omitempty"`
	Automations     *Automations `json:"automations,omitempty"`
	Connected       bool         `json:"connected"`
	ConnectedRepoID string       `json:"connected_repo_id,omitempty"`
	AttributionV2   *bool        `json:"attribution_v2,omitempty"`
	WorkspaceFreeze *bool        `json:"workspace_freeze,omitempty"`
}

var settingsKnownKeys = []string{
	"enabled", "version", "providers", "trailers", "automations",
	"connected", "connected_repo_id", "attribution_v2", "workspace_freeze",
}

func (s *Settings) UnmarshalJSON(b []byte) error {
	var known settingsKnown
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range settingsKnownKeys {
		delete(raw, k)
	}
	*s = Settings{
		Enabled:         known.Enabled,
		Version:         known.Version,
		Providers:       known.Providers,
		Trailers:        known.Trailers,
		Automations:     known.Automations,
		Connected:       known.Connected,
		ConnectedRepoID: known.ConnectedRepoID,
		AttributionV2:   known.AttributionV2,
		WorkspaceFreeze: known.WorkspaceFreeze,
		extra:           raw,
	}
	return nil
}

func (s Settings) MarshalJSON() ([]byte, error) {
	kb, err := json.Marshal(settingsKnown{
		Enabled:         s.Enabled,
		Version:         s.Version,
		Providers:       s.Providers,
		Trailers:        s.Trailers,
		Automations:     s.Automations,
		Connected:       s.Connected,
		ConnectedRepoID: s.ConnectedRepoID,
		AttributionV2:   s.AttributionV2,
		WorkspaceFreeze: s.WorkspaceFreeze,
	})
	if err != nil {
		return nil, err
	}
	if len(s.extra) == 0 {
		return kb, nil
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(kb, &out); err != nil {
		return nil, err
	}
	for k, v := range s.extra {
		out[k] = v
	}
	return json.Marshal(out)
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

// WriteSettings atomically replaces settings.json and updates the enabled
// marker used by hooks that cannot parse JSON.
func WriteSettings(semDir string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := platform.WriteFileAtomic(SettingsPath(semDir), data, 0o644); err != nil {
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

// AttributionV2Enabled reports whether attribution v2 is active. The environment
// overrides repository settings. Missing settings default to v2; unreadable
// settings fail closed to v1.
func AttributionV2Enabled(semDir string) bool {
	switch os.Getenv("SEMANTICA_ATTRIBUTION_V2") {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	s, err := ReadSettings(semDir)
	if err != nil {
		return false
	}
	if s.AttributionV2 == nil {
		return true
	}
	return *s.AttributionV2
}

// WorkspaceFreezeEnabled reports whether pre-commit workspace observations are
// enabled. The environment overrides repository settings; errors and missing
// values disable it.
func WorkspaceFreezeEnabled(semDir string) bool {
	switch os.Getenv("SEMANTICA_WORKSPACE_FREEZE") {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	s, err := ReadSettings(semDir)
	if err != nil {
		return false
	}
	if s.WorkspaceFreeze == nil {
		return false
	}
	return *s.WorkspaceFreeze
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
