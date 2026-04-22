package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistPath_HonorsHome(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir consults HOME on Unix and USERPROFILE on
	// Windows. Set both so the test exercises the same code path
	// on every platform the CI runs on.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", "sh.semantica.worker.plist")
	if got != want {
		t.Errorf("PlistPath = %q, want %q", got, want)
	}
}

func TestWorkerLogPath_HonorsSemanticaHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	got, err := WorkerLogPath()
	if err != nil {
		t.Fatalf("WorkerLogPath: %v", err)
	}
	want := filepath.Join(base, "worker-launcher.log")
	if got != want {
		t.Errorf("WorkerLogPath = %q, want %q", got, want)
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

func TestDomainTarget_IsUserDomainPlusLabel(t *testing.T) {
	got := DomainTarget()
	want := fmt.Sprintf("gui/%d/%s", os.Getuid(), LabelWorker)
	if got != want {
		t.Errorf("DomainTarget = %q, want %q", got, want)
	}
}
