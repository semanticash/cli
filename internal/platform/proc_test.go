package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachProcess_SetsAttributes(t *testing.T) {
	cmd := exec.Command("echo", "test")
	DetachProcess(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set after DetachProcess")
	}
}

func TestSetProcessGroup_SetsAttributes(t *testing.T) {
	cmd := exec.Command("echo", "test")
	SetProcessGroup(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set after SetProcessGroup")
	}
}

func TestDetachProcess_IntegrationChildSurvivesParent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: detached process survival test")
	}
	target := filepath.Join(t.TempDir(), "survivor.txt")

	cmd := startHelper(t, "detachparent", "SEMANTICA_HELPER_TARGET_FILE="+target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("intermediate process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SPAWNED") {
		t.Fatalf("intermediate never spawned a detached child:\n%s", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(target)
		if err == nil {
			if string(got) != "alive-after-parent-exit" {
				t.Fatalf("target file = %q, want alive-after-parent-exit", got)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached child did not survive parent exit: target file never written")
}
