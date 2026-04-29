//go:build linux

package launcher

import (
	"path/filepath"
	"strings"
	"testing"
)

// UnitPath honors XDG_CONFIG_HOME and falls back to $HOME/.config
// when XDG_CONFIG_HOME is unset, per XDG conventions.
func TestUnitPath_HonorsXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	want := filepath.Join(xdg, "systemd", "user", "sh.semantica.worker.service")
	if got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
}

func TestUnitPath_FallsBackToHomeConfigWhenXDGUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	got, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	want := filepath.Join(home, ".config", "systemd", "user", "sh.semantica.worker.service")
	if got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
}

// UnitTarget on Linux is the systemd unit basename (label +
// ".service"). It must be a string systemctl --user accepts as a
// unit argument.
func TestUnitTarget_ReturnsServiceName(t *testing.T) {
	got := UnitTarget()
	want := "sh.semantica.worker.service"
	if got != want {
		t.Errorf("UnitTarget = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, ".service") {
		t.Errorf("UnitTarget must end with .service, got %q", got)
	}
}

// UserDomain on Linux returns "" because the systemd user instance
// has no analog to launchctl's gui/<uid> tuple. Pinned so a future
// refactor that mistakenly returns a non-empty value is caught.
func TestUserDomain_EmptyOnLinux(t *testing.T) {
	if got := UserDomain(); got != "" {
		t.Errorf("UserDomain = %q, want empty string", got)
	}
}
