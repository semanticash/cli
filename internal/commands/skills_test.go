package commands

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsCmd_ParentAndBackingAreHidden locks the wiring invariant
// that the `skills` parent and its backing subcommands stay out of
// help output until `install` / `uninstall` ship.
func TestSkillsCmd_ParentAndBackingAreHidden(t *testing.T) {
	root := NewRootCmd()
	skills, _, err := root.Find([]string{"skills"})
	if err != nil {
		t.Fatalf("skills command not found on root: %v", err)
	}
	if !skills.Hidden {
		t.Errorf("skills parent should be Hidden until install/uninstall ship")
	}

	handoff, _, err := root.Find([]string{"skills", "handoff"})
	if err != nil {
		t.Fatalf("skills handoff command not found: %v", err)
	}
	if !handoff.Hidden {
		t.Errorf("skills handoff backing command must be Hidden")
	}
}

// TestSkillsHandoff_StaysInLockstepWithUserFacing locks the contract
// that the hidden `semantica skills handoff` and the user-facing
// `semantica handoff --write` produce identical output. The backing
// command is intentionally a thin wrapper; this test catches output
// or error drift between the two surfaces.
func TestSkillsHandoff_StaysInLockstepWithUserFacing(t *testing.T) {
	repo := initBareGitRepoForTest(t)
	t.Setenv("SEMANTICA_HOME", filepath.Join(t.TempDir(), "sem-home"))

	userOut, userErr := runRoot(t, []string{"handoff", "--repo", repo, "--write"})
	skillOut, skillErr := runRoot(t, []string{"skills", "handoff", "--repo", repo})

	if userErr == nil || skillErr == nil {
		t.Fatalf("expected ErrNoSession on both surfaces; user=%v skill=%v", userErr, skillErr)
	}
	if userErr.Error() != skillErr.Error() {
		t.Errorf("error drift between user-facing and skill backing:\nuser:  %q\nskill: %q",
			userErr.Error(), skillErr.Error())
	}
	if userOut != skillOut {
		t.Errorf("stdout drift between user-facing and skill backing:\nuser:  %q\nskill: %q",
			userOut, skillOut)
	}
}

// TestSkillsHandoff_NoUsageOrErrorBlockOnRunError mirrors the
// invariant pinned for other hidden commands: SilenceUsage and
// SilenceErrors must keep cobra's noise out of stdout/stderr so
// SKILL.md bodies receive only the message we shape.
func TestSkillsHandoff_NoUsageOrErrorBlockOnRunError(t *testing.T) {
	repo := initBareGitRepoForTest(t)
	t.Setenv("SEMANTICA_HOME", filepath.Join(t.TempDir(), "sem-home"))

	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"skills", "handoff", "--repo", repo})

	if err := root.Execute(); err == nil {
		t.Fatal("expected ErrNoSession, got nil")
	}

	out := buf.String()
	if strings.Contains(out, "Usage:") {
		t.Errorf("usage block leaked despite SilenceUsage:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "Error:") {
		t.Errorf("cobra printed its own Error: line despite SilenceErrors:\n%s", out)
	}
}

// runRoot executes the root command with the supplied args, captures
// stdout, and returns stdout and the RunE error. Stderr is folded
// into the same buffer so silence-invariant tests can inspect it.
func runRoot(t *testing.T, args []string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// initBareGitRepoForTest returns a temp directory that is a real git
// repo so handoff.Service.Write can resolve a repo root.
func initBareGitRepoForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}
	return canonical
}
