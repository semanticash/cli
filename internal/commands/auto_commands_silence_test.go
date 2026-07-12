package commands

import (
	"bytes"
	"strings"
	"testing"
)

// Hidden background commands should not print Cobra usage or
// `Error:` blocks on RunE failures.

func TestAutoPlaybook_NoUsageOrErrorBlockOnRunError(t *testing.T) {
	cmd := NewAutoPlaybookCmd()
	var buf bytes.Buffer
	cmd.SetErr(&buf)
	cmd.SetOut(&buf)

	// Force a RunE failure instead of a flag-validation failure.
	cmd.SetArgs([]string{
		"--checkpoint", "c1",
		"--commit", "deadbeef",
		"--repo", t.TempDir() + "/not-a-real-repo",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected RunE error for missing repo, got nil")
	}

	out := buf.String()
	if strings.Contains(out, "Usage:") {
		t.Errorf("cobra usage block leaked despite SilenceUsage. output:\n%s", out)
	}
	if strings.Contains(out, "Flags:") {
		t.Errorf("cobra flags block leaked despite SilenceUsage. output:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "Error:") {
		t.Errorf("cobra printed its own 'Error:' line despite SilenceErrors. output:\n%s", out)
	}
}
