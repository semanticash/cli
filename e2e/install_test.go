//go:build e2e

package e2e_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBinaryVersion(t *testing.T) {
	cmd := exec.Command(semBinary, "--version")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--version failed: %v", err)
	}
	if !strings.Contains(string(out), "semantica") {
		t.Errorf("--version output missing 'semantica': %s", out)
	}
}

func TestBinaryHelp(t *testing.T) {
	cmd := exec.Command(semBinary, "--help")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help failed: %v", err)
	}
	output := string(out)
	for _, want := range []string{"enable", "explain"} {
		if !strings.Contains(output, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}
