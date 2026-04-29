package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/launcher"
)

// These tests cover the hint branches in printLauncherStatus.

// Unsupported hosts should not be nudged toward launcher enable.
func TestPrintLauncherStatus_NonDarwinDoesNotNudgeToLauncherEnable(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "linux",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/n/a",
		UnitTarget:       "gui/0/sh.semantica.worker",
		ServiceState:     "unsupported",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if strings.Contains(out, "semantica launcher enable") {
		t.Errorf("unsupported-OS output must not suggest 'launcher enable', got:\n%s", out)
	}
	if !strings.Contains(out, "only available on macOS") {
		t.Errorf("expected explicit macOS-only hint, got:\n%s", out)
	}
}

// Disabled state on macOS should show the opt-in hint.
func TestPrintLauncherStatus_DisabledOnDarwinSuggestsLauncherEnable(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "darwin",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
		UnitTarget:       "gui/501/sh.semantica.worker",
		ServiceState:     "not loaded",
		LogPath:          "/Users/test/.semantica/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "semantica launcher enable") {
		t.Errorf("disabled-on-darwin output should suggest 'launcher enable', got:\n%s", out)
	}
}

// A settings error should be rendered prominently and suppress the
// generic opt-in hint.
func TestPrintLauncherStatus_SettingsErrorSurfacesProminently(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "darwin",
		SettingsError:    "parse /Users/test/.semantica/settings.json: invalid character 'n'",
		ExpectedUnitPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
		UnitTarget:       "gui/501/sh.semantica.worker",
		ServiceState:     "not loaded",
		LogPath:          "/Users/test/.semantica/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "Settings error:") {
		t.Errorf("expected 'Settings error:' label in output, got:\n%s", out)
	}
	if !strings.Contains(out, "invalid character") {
		t.Errorf("expected the settings error detail in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Fix or remove the settings file") {
		t.Errorf("expected recovery hint for settings error, got:\n%s", out)
	}
	// The generic opt-in hint should be suppressed.
	if strings.Contains(out, "to opt in") {
		t.Errorf("opt-in hint must be suppressed when SettingsError is set, got:\n%s", out)
	}
}

// Unsupported hosts should prefer the macOS-only hint over the
// settings-error recovery hint.
func TestPrintLauncherStatus_UnsupportedHostBeatsSettingsErrorHint(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "linux",
		SettingsError:    "parse settings.json: invalid character",
		ExpectedUnitPath: "/n/a",
		UnitTarget:       "gui/0/sh.semantica.worker",
		ServiceState:     "unsupported",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "only available on macOS") {
		t.Errorf("expected macOS-only hint on unsupported host, got:\n%s", out)
	}
	if strings.Contains(out, "semantica launcher enable") {
		t.Errorf("unsupported host must not recommend 'launcher enable' even with a settings error, got:\n%s", out)
	}
	if strings.Contains(out, "Fix or remove the settings file") {
		t.Errorf("settings-error recovery hint must be suppressed on unsupported host, got:\n%s", out)
	}
	// The labeled error line should still be present.
	if !strings.Contains(out, "Settings error:") {
		t.Errorf("settings-error label must still appear on unsupported host, got:\n%s", out)
	}
}

// Drift cases should still produce their specific hints.
func TestPrintLauncherStatus_DriftHintsStillFire(t *testing.T) {
	// settings enabled + daemon manager not loaded
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:              "darwin",
		SettingsEnabled: true,
		LoadedInDaemon:  false,
		ServiceState:    "not loaded",
		UnitOnDisk:      true,
	})
	if !strings.Contains(buf.String(), "settings say enabled, but the OS daemon manager") {
		t.Errorf("expected drift hint, got:\n%s", buf.String())
	}

	buf.Reset()
	// settings disabled + daemon manager loaded
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:              "darwin",
		SettingsEnabled: false,
		LoadedInDaemon:  true,
		ServiceState:    "loaded",
	})
	if !strings.Contains(buf.String(), "the OS daemon manager has the service loaded") {
		t.Errorf("expected reverse drift hint, got:\n%s", buf.String())
	}

	buf.Reset()
	// settings enabled + unit file missing
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:              "darwin",
		SettingsEnabled: true,
		LoadedInDaemon:  true,
		ServiceState:    "loaded",
		UnitOnDisk:      false,
	})
	if !strings.Contains(buf.String(), "unit file is missing") {
		t.Errorf("expected unit-missing drift hint, got:\n%s", buf.String())
	}
}
