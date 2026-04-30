//go:build darwin

package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitPath_HonorsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", "sh.semantica.worker.plist")
	if got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
}

func TestUserDomain_IncludesCurrentUID(t *testing.T) {
	got := UserDomain()
	want := fmt.Sprintf("gui/%d", os.Getuid())
	if got != want {
		t.Errorf("UserDomain = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "gui/") {
		t.Errorf("UserDomain must use gui/<uid> form, got %q", got)
	}
}

func TestUnitTarget_IsUserDomainPlusLabel(t *testing.T) {
	got := UnitTarget()
	want := fmt.Sprintf("gui/%d/%s", os.Getuid(), LabelWorker)
	if got != want {
		t.Errorf("UnitTarget = %q, want %q", got, want)
	}
}
