package commands

import (
	"bytes"
	"strings"
	"testing"
)

// These hidden background commands should not let cobra print a usage
// block or its own "Error:" line on RunE failures.

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

func TestAutoImplementationSummary_NoUsageOrErrorBlockOnRunError(t *testing.T) {
	// Use an isolated home so the command cannot reuse a real
	// implementations DB from the developer machine.
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	cmd := NewAutoImplementationSummaryCmd()
	var buf bytes.Buffer
	cmd.SetErr(&buf)
	cmd.SetOut(&buf)

	// With no DB file in the isolated home, RunE fails deterministically.
	cmd.SetArgs([]string{"--impl", "00000000-0000-0000-0000-000000000000"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected RunE error for missing implementations DB, got nil")
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
