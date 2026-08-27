package util

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsPlaybookEnabled_NoAutomations(t *testing.T) {
	dir := t.TempDir()
	// Write settings without automations field.
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if IsPlaybookEnabled(dir) {
		t.Error("expected false when automations is nil")
	}
}

func TestIsPlaybookEnabled_Disabled(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
		Automations: &Automations{
			Playbook: PlaybookAutomation{Enabled: false},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if IsPlaybookEnabled(dir) {
		t.Error("expected false when playbook.enabled is false")
	}
}

func TestIsPlaybookEnabled_Enabled(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
		Automations: &Automations{
			Playbook: PlaybookAutomation{Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if !IsPlaybookEnabled(dir) {
		t.Error("expected true when playbook.enabled is true")
	}
}

func TestIsPlaybookEnabled_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if IsPlaybookEnabled(dir) {
		t.Error("expected false when settings.json doesn't exist")
	}
}

func TestReadSettings_RoundTripsAutomations(t *testing.T) {
	dir := t.TempDir()
	original := Settings{
		Enabled:   true,
		Version:   1,
		Providers: []string{"claude-code"},
		Automations: &Automations{
			Playbook: PlaybookAutomation{Enabled: true},
		},
	}

	if err := WriteSettings(dir, original); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Enabled {
		t.Error("enabled should be true")
	}
	if got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
	if len(got.Providers) != 1 || got.Providers[0] != "claude-code" {
		t.Errorf("providers = %v, want [claude-code]", got.Providers)
	}
	if got.Automations == nil {
		t.Fatal("automations should not be nil")
	}
	if !got.Automations.Playbook.Enabled {
		t.Error("automations.playbook.enabled should be true")
	}
}

func TestSettings_PreservesUnknownKeysAtAllLevels(t *testing.T) {
	dir := t.TempDir()
	// Include unknown fields at every structured level.
	input := `{
  "enabled": true,
  "version": 3,
  "future_top": {"nested": [1, 2]},
  "automations": {
    "playbook": {"enabled": true, "future_playbook": "keep-me"},
    "future_automation": {"enabled": false}
  }
}`
	if err := os.WriteFile(SettingsPath(dir), []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 3 {
		t.Errorf("version = %d, want 3", s.Version)
	}

	// Mutate a known field before the round trip.
	s.Automations.Playbook.Enabled = false
	if err := WriteSettings(dir, s); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(SettingsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	// Compare unknown values after normalizing whitespace.
	compact := func(raw json.RawMessage) string {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatalf("compact %s: %v", raw, err)
		}
		return buf.String()
	}
	if compact(raw["future_top"]) != `{"nested":[1,2]}` {
		t.Errorf("top-level unknown key = %s, want preserved", raw["future_top"])
	}
	var auto map[string]json.RawMessage
	if err := json.Unmarshal(raw["automations"], &auto); err != nil {
		t.Fatal(err)
	}
	if compact(auto["future_automation"]) != `{"enabled":false}` {
		t.Errorf("automations unknown key = %s, want preserved", auto["future_automation"])
	}
	var pb map[string]json.RawMessage
	if err := json.Unmarshal(auto["playbook"], &pb); err != nil {
		t.Fatal(err)
	}
	if compact(pb["future_playbook"]) != `"keep-me"` {
		t.Errorf("playbook unknown key = %s, want preserved", pb["future_playbook"])
	}
	if string(pb["enabled"]) != "false" {
		t.Errorf("playbook.enabled = %s, want mutated to false", pb["enabled"])
	}

	// Confirm the rewritten settings remain readable.
	again, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != 3 || again.Automations.Playbook.Enabled {
		t.Errorf("re-read = %+v, want version 3 and playbook disabled", again)
	}
}

func TestWorkspaceFreezeEnabled(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	t.Run("default off", func(t *testing.T) {
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "")
		if WorkspaceFreezeEnabled(t.TempDir()) {
			t.Error("expected off with no settings and no env")
		}
	})

	t.Run("env on overrides default", func(t *testing.T) {
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "1")
		if !WorkspaceFreezeEnabled(t.TempDir()) {
			t.Error("expected on with env=1 and no settings")
		}
	})

	t.Run("settings on", func(t *testing.T) {
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "")
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, WorkspaceFreeze: boolPtr(true)}); err != nil {
			t.Fatal(err)
		}
		if !WorkspaceFreezeEnabled(dir) {
			t.Error("expected on from settings field")
		}
	})

	t.Run("settings off explicit", func(t *testing.T) {
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "")
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, WorkspaceFreeze: boolPtr(false)}); err != nil {
			t.Fatal(err)
		}
		if WorkspaceFreezeEnabled(dir) {
			t.Error("expected off from explicit settings false")
		}
	})

	t.Run("malformed settings fail off", func(t *testing.T) {
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "")
		dir := t.TempDir()
		if err := os.WriteFile(SettingsPath(dir), []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if WorkspaceFreezeEnabled(dir) {
			t.Error("expected off when settings cannot be read")
		}
	})

	t.Run("env off overrides settings on", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, WorkspaceFreeze: boolPtr(true)}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "0")
		if WorkspaceFreezeEnabled(dir) {
			t.Error("expected env=0 to override settings on")
		}
	})

	t.Run("env on overrides settings off", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, WorkspaceFreeze: boolPtr(false)}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "1")
		if !WorkspaceFreezeEnabled(dir) {
			t.Error("expected env=1 to override settings off")
		}
	})
}

func TestAttributionV2Enabled(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	t.Run("default on", func(t *testing.T) {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "")
		if !AttributionV2Enabled(t.TempDir()) {
			t.Error("expected on with no settings and no env")
		}
	})

	t.Run("env off overrides default", func(t *testing.T) {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
		if AttributionV2Enabled(t.TempDir()) {
			t.Error("expected off with env=0 and no settings")
		}
	})

	t.Run("settings on", func(t *testing.T) {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "")
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, AttributionV2: boolPtr(true)}); err != nil {
			t.Fatal(err)
		}
		if !AttributionV2Enabled(dir) {
			t.Error("expected on from settings field")
		}
	})

	t.Run("settings off explicit", func(t *testing.T) {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "")
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, AttributionV2: boolPtr(false)}); err != nil {
			t.Fatal(err)
		}
		if AttributionV2Enabled(dir) {
			t.Error("expected off from explicit settings false")
		}
	})

	t.Run("malformed settings fail closed to v1", func(t *testing.T) {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "")
		dir := t.TempDir()
		if err := os.WriteFile(SettingsPath(dir), []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if AttributionV2Enabled(dir) {
			t.Error("expected v1 when settings cannot be read")
		}
	})

	t.Run("env overrides settings", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteSettings(dir, Settings{Enabled: true, Version: 1, AttributionV2: boolPtr(true)}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
		if AttributionV2Enabled(dir) {
			t.Error("expected env=0 to override settings on")
		}
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
		if !AttributionV2Enabled(dir) {
			t.Error("expected env=1 on")
		}
	})
}

func TestIsConnected_Default(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if IsConnected(dir) {
		t.Error("expected false when connected not set (zero value)")
	}
}

func TestIsConnected_True(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled:   true,
		Version:   1,
		Connected: true,
	}); err != nil {
		t.Fatal(err)
	}

	if !IsConnected(dir) {
		t.Error("expected true when connected is true")
	}
}

func TestIsConnected_False(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled:   true,
		Version:   1,
		Connected: false,
	}); err != nil {
		t.Fatal(err)
	}

	if IsConnected(dir) {
		t.Error("expected false when connected is explicitly false")
	}
}

func TestIsConnected_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if IsConnected(dir) {
		t.Error("expected false when settings.json doesn't exist")
	}
}

func TestReadSettings_RoundTripsConnected(t *testing.T) {
	dir := t.TempDir()
	original := Settings{
		Enabled:   true,
		Version:   1,
		Connected: true,
	}

	if err := WriteSettings(dir, original); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Connected {
		t.Error("connected should round-trip as true")
	}
}

func TestReadSettings_OmitsAutomationsWhenNil(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Read the raw JSON to confirm automations key is absent.
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(data), "automations") {
		t.Error("expected automations key to be omitted from JSON when nil")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
