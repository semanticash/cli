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

// Existing install with an automations block but no implementation_summary key.
// Should backfill to true and persist.
func TestIsImplementationSummaryEnabled_BackfillMissingKey(t *testing.T) {
	dir := t.TempDir()
	// Simulate what an older binary wrote: automations with only playbook,
	// no implementation_summary key at all. Write raw JSON to avoid the
	// current struct serializing the zero-value field.
	rawJSON := `{"enabled":true,"version":1,"automations":{"playbook":{"enabled":true}}}`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(rawJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write the enabled marker so WriteSettings doesn't remove it.
	if err := os.WriteFile(filepath.Join(dir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// First read should backfill and return true.
	if !IsImplementationSummaryEnabled(dir) {
		t.Error("expected true on first read (backfilled)")
	}

	// Verify it was persisted.
	s, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Automations.ImplementationSummary.Enabled {
		t.Error("backfilled value should be persisted as true")
	}
	// Playbook should be unchanged.
	if !s.Automations.Playbook.Enabled {
		t.Error("playbook.enabled should not be affected by backfill")
	}
}

// Very old install with no automations block at all. Should create the block
// with both defaults and return true.
func TestIsImplementationSummaryEnabled_BackfillNilAutomations(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
		// No Automations field at all.
	}); err != nil {
		t.Fatal(err)
	}

	if !IsImplementationSummaryEnabled(dir) {
		t.Error("expected true on first read (nil automations backfilled)")
	}

	// Verify both automations were written with correct defaults.
	s, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Automations == nil {
		t.Fatal("automations should have been created")
	}
	if !s.Automations.ImplementationSummary.Enabled {
		t.Error("implementation_summary.enabled should be true")
	}
	if !s.Automations.Playbook.Enabled {
		t.Error("playbook.enabled should also be true (matching enable defaults)")
	}
}

// Setting explicitly disabled by user. Should stay false, no backfill.
func TestIsImplementationSummaryEnabled_ExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
		Automations: &Automations{
			Playbook:              PlaybookAutomation{Enabled: true},
			ImplementationSummary: ImplementationSummaryAutomation{Enabled: false},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if IsImplementationSummaryEnabled(dir) {
		t.Error("expected false when explicitly disabled")
	}
}

// New install with both values present. Should return true directly.
func TestIsImplementationSummaryEnabled_NewInstall(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{
		Enabled: true,
		Version: 1,
		Automations: &Automations{
			Playbook:              PlaybookAutomation{Enabled: true},
			ImplementationSummary: ImplementationSummaryAutomation{Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if !IsImplementationSummaryEnabled(dir) {
		t.Error("expected true for new install")
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
