package util

import (
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
