//go:build windows

package launcher

import (
	"path/filepath"
	"strings"
	"testing"
)

// UnitPath stores the Task Scheduler XML under broker.GlobalBase.
// SEMANTICA_HOME overrides the base, so the test can pin a known
// directory without depending on the user profile.
func TestUnitPath_HonorsSemanticaHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	got, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	want := filepath.Join(base, "sh.semantica.worker.xml")
	if got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
}

// UnitTarget on Windows is the Task Scheduler task name with
// folder prefix. schtasks.exe accepts this string as /TN.
func TestUnitTarget_ReturnsTaskName(t *testing.T) {
	got := UnitTarget()
	want := `\Semantica\sh.semantica.worker`
	if got != want {
		t.Errorf("UnitTarget = %q, want %q", got, want)
	}
	// Folder prefix is required by Task Scheduler convention.
	if !strings.HasPrefix(got, `\Semantica\`) {
		t.Errorf("UnitTarget must start with \\Semantica\\ folder prefix, got %q", got)
	}
}

// UserDomain returns "" on Windows because Task Scheduler addresses
// tasks by name, not by user/UID tuple.
func TestUserDomain_EmptyOnWindows(t *testing.T) {
	if got := UserDomain(); got != "" {
		t.Errorf("UserDomain = %q, want empty string", got)
	}
}
