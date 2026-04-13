package platform

import (
	"os/exec"
	"testing"
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
	t.Skip("TODO: implement child-survives-parent test in Phase 1")
}
