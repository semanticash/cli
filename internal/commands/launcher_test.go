package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/launcher"
)

// These tests cover the hint branches in printLauncherStatus.

// Hosts whose backend is unreachable (whatever the reason) must
// not be nudged toward 'launcher enable'. The hint should describe
// the actual problem on the actual host rather than denying support
// for the OS.
func TestPrintLauncherStatus_LinuxUnreachableHintsAtSystemdNotMacOS(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "linux",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/home/test/.config/systemd/user/sh.semantica.worker.service",
		UnitTarget:       "sh.semantica.worker.service",
		ServiceState:     "unsupported",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if strings.Contains(out, "semantica launcher enable") {
		t.Errorf("unsupported-state output must not suggest 'launcher enable', got:\n%s", out)
	}
	// Linux hosts must NOT see a macOS-only message - the backend
	// exists for this OS, the runtime environment is the issue.
	if strings.Contains(out, "only available on macOS") {
		t.Errorf("linux host must not see the macOS-only hint, got:\n%s", out)
	}
	if !strings.Contains(out, "systemd user instance") {
		t.Errorf("expected hint pointing at the systemd user instance, got:\n%s", out)
	}
}

// On Linux, daemon-manager failures land in ServiceState as
// "error: <msg>" (Status maps systemctl errors that way). The
// hint logic must route those into the OS-aware unsupportedHint
// branch so users see "systemd user instance is not reachable"
// rather than a misleading "Run 'semantica launcher enable'"
// fallback. Regression test for the bug where the linux-specific
// hint was unreachable in the real Status flow.
func TestPrintLauncherStatus_LinuxDaemonErrorRoutesToSystemdHint(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "linux",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/home/test/.config/systemd/user/sh.semantica.worker.service",
		UnitTarget:       "sh.semantica.worker.service",
		ServiceState:     "error: systemctl --user is-active: exit 1: Failed to connect to bus",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "systemd user instance") {
		t.Errorf("linux daemon-manager error must route to the systemd hint, got:\n%s", out)
	}
	// Must NOT fall through to the disabled-state opt-in nudge -
	// 'launcher enable' would fail for the same reason 'is-active'
	// did, which is exactly the wrong-direction case the hint logic
	// is meant to prevent.
	if strings.Contains(out, "to opt in") {
		t.Errorf("linux daemon-manager error must not nudge to 'launcher enable', got:\n%s", out)
	}
}

// Symmetric coverage on darwin: launchctl errors should also route
// to the OS-aware hint rather than the opt-in fallback.
func TestPrintLauncherStatus_DarwinDaemonErrorRoutesToLaunchdHint(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "darwin",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/Users/test/Library/LaunchAgents/sh.semantica.worker.plist",
		UnitTarget:       "gui/501/sh.semantica.worker",
		ServiceState:     "error: launchctl print: exit 9: Unrecognized target specifier",
		LogPath:          "/Users/test/.semantica/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "launchd is not reachable") {
		t.Errorf("darwin daemon-manager error must route to the launchd hint, got:\n%s", out)
	}
	if strings.Contains(out, "to opt in") {
		t.Errorf("darwin daemon-manager error must not nudge to 'launcher enable', got:\n%s", out)
	}
}

// Truly unsupported OSes (no backend at compile time) get the
// neutral message that names the supported set.
func TestPrintLauncherStatus_OtherOSDescribesSupportedBackends(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "freebsd",
		SettingsEnabled:  false,
		ExpectedUnitPath: "/n/a",
		UnitTarget:       "",
		ServiceState:     "unsupported",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if strings.Contains(out, "semantica launcher enable") {
		t.Errorf("unsupported-OS output must not suggest 'launcher enable', got:\n%s", out)
	}
	if !strings.Contains(out, "no backend on this OS") {
		t.Errorf("expected explicit 'no backend' hint for unsupported OS, got:\n%s", out)
	}
	if !strings.Contains(out, "macOS") || !strings.Contains(out, "Linux") {
		t.Errorf("hint should name the supported backends, got:\n%s", out)
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

// Unsupported state takes precedence over a settings-error
// recovery hint: the launcher cannot be re-enabled here regardless,
// so the OS-appropriate unsupported message wins.
func TestPrintLauncherStatus_UnsupportedHostBeatsSettingsErrorHint(t *testing.T) {
	var buf bytes.Buffer
	printLauncherStatus(&buf, launcher.StatusResult{
		OS:               "linux",
		SettingsError:    "parse settings.json: invalid character",
		ExpectedUnitPath: "/n/a",
		UnitTarget:       "sh.semantica.worker.service",
		ServiceState:     "unsupported",
		LogPath:          "/tmp/worker-launcher.log",
	})
	out := buf.String()

	if !strings.Contains(out, "systemd user instance") {
		t.Errorf("expected linux-appropriate unsupported hint on linux host, got:\n%s", out)
	}
	if strings.Contains(out, "semantica launcher enable") {
		t.Errorf("unsupported state must not recommend 'launcher enable' even with a settings error, got:\n%s", out)
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
