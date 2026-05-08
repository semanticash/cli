package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsCmd_VisibilityWiring locks the wiring invariant for
// the `skills` subtree once install/uninstall ship: the parent and
// install/uninstall are visible to terminal users, while the
// per-skill backing commands stay hidden because SKILL.md bodies
// invoke them directly.
func TestSkillsCmd_VisibilityWiring(t *testing.T) {
	root := NewRootCmd()

	skills, _, err := root.Find([]string{"skills"})
	if err != nil {
		t.Fatalf("skills command not found on root: %v", err)
	}
	if skills.Hidden {
		t.Errorf("skills parent should be visible now that install/uninstall ship")
	}

	for _, name := range []string{"install", "uninstall"} {
		c, _, err := root.Find([]string{"skills", name})
		if err != nil {
			t.Fatalf("skills %s not found: %v", name, err)
		}
		if c.Hidden {
			t.Errorf("skills %s should be visible to terminal users", name)
		}
	}

	for _, name := range []string{"handoff", "explain"} {
		c, _, err := root.Find([]string{"skills", name})
		if err != nil {
			t.Fatalf("skills %s backing command not found: %v", name, err)
		}
		if !c.Hidden {
			t.Errorf("skills %s backing command must remain Hidden", name)
		}
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

// --- skills explain command-level contract ---
//
// SKILL.md consumers depend on the cobra wiring as much as the
// engine: structured modes (including not-found and blocked) must
// exit zero with valid JSON on stdout, and runtime failures must
// not leak cobra's usage / error noise into the output the agent
// reads. These tests pin those invariants alongside the engine
// tests in internal/explain/.

// TestSkillsExplain_UnsafeRefExitsZeroWithJSON exercises the
// not-found branch: an unsafe ref must return structured JSON and
// exit code zero so the SKILL.md body parses the message rather
// than treating it as a runtime error.
func TestSkillsExplain_UnsafeRefExitsZeroWithJSON(t *testing.T) {
	repo := initBareGitRepoForTest(t)
	out, err := runRoot(t, []string{"--repo", repo, "skills", "explain", "main; rm -rf /"})
	if err != nil {
		t.Fatalf("expected zero-exit (structured JSON), got err: %v\nstdout:\n%s", err, out)
	}
	parsed := mustParseJSON(t, out)
	if got := parsed["mode"]; got != "not-found" {
		t.Errorf("mode = %v, want not-found", got)
	}
	if got := parsed["reason"]; got != "ref_unsafe" {
		t.Errorf("reason = %v, want ref_unsafe", got)
	}
	if _, ok := parsed["message"]; !ok {
		t.Errorf("message field missing for not-found / ref_unsafe: %s", out)
	}
}

// TestSkillsExplain_GitOnlyExitsZeroWithJSON exercises the happy
// path through cobra: HEAD on a real commit returns mode=git-only,
// fallback_reason=remote_not_attempted, and a non-empty diff
// excerpt, which is the contract a SKILL.md body relies on for layer 3.
func TestSkillsExplain_GitOnlyExitsZeroWithJSON(t *testing.T) {
	repo := initRepoWithCommitForTest(t, "feat: command contract", "package main\nfunc Run() {}\n")
	out, err := runRoot(t, []string{"--repo", repo, "skills", "explain", "HEAD"})
	if err != nil {
		t.Fatalf("expected zero-exit, got err: %v\nstdout:\n%s", err, out)
	}
	parsed := mustParseJSON(t, out)
	if got := parsed["mode"]; got != "git-only" {
		t.Fatalf("mode = %v, want git-only\nstdout:\n%s", got, out)
	}
	if got := parsed["fallback_reason"]; got != "remote_not_attempted" {
		t.Errorf("fallback_reason = %v, want remote_not_attempted", got)
	}
	if _, ok := parsed["commit_metadata"].(map[string]any); !ok {
		t.Errorf("commit_metadata missing or wrong shape: %s", out)
	}
	if got, _ := parsed["diff_excerpt"].(string); got == "" {
		t.Errorf("diff_excerpt empty: %s", out)
	}
}

// TestSkillsExplain_RuntimeFailureIsNonZeroAndQuiet pins the other
// half of the contract: when the command itself can't produce
// structured output (here, `--repo` points at a non-git path), the
// process exits non-zero and the output does not contain cobra's
// usage block or its `Error:` prefix.
func TestSkillsExplain_RuntimeFailureIsNonZeroAndQuiet(t *testing.T) {
	notARepo := t.TempDir()
	out, err := runRoot(t, []string{"--repo", notARepo, "skills", "explain", "HEAD"})
	if err == nil {
		t.Fatalf("expected runtime failure for non-git repo, got nil err\nstdout:\n%s", out)
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("usage block leaked despite SilenceUsage:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "Error:") {
		t.Errorf("cobra printed its own Error: line despite SilenceErrors:\n%s", out)
	}
}

// mustParseJSON unmarshals the captured stdout and fails the test
// with a clear pointer if it isn't valid JSON. The skill body
// would error out the same way; pinning this here means a future
// change that prints anything before the JSON line gets caught.
func mustParseJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(raw)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nraw:\n%s", err, raw)
	}
	return parsed
}

// initRepoWithCommitForTest creates a temp git repo with one
// commit so `skills explain HEAD` resolves through the happy path.
// Mirrors the helper in internal/explain/explain_test.go but lives
// in this package so the command-level tests stay self-contained.
func initRepoWithCommitForTest(t *testing.T, message, fileBody string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd("init", ".")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "main.go")
	gitCmd("commit", "-m", message)
	return dir
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
