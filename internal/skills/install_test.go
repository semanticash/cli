package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installTestSetup configures a hermetic environment for install /
// uninstall tests: a source tree containing the canonical sample
// skill, and a SEMANTICA_CLAUDE_SKILLS_DIR pointing at a temp dest.
// Returns the source root and the destination root.
func installTestSetup(t *testing.T) (srcRoot, dstRoot string) {
	t.Helper()
	srcRoot = filepath.Join(t.TempDir(), "src")
	dstRoot = filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	skillDir := filepath.Join(srcRoot, "semantica-handoff")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(sampleSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	return srcRoot, dstRoot
}

func TestInstall_FreshInstallStampsAndWritesFile(t *testing.T) {
	src, dst := installTestSetup(t)

	rep, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Action != ActionInstalled {
		t.Fatalf("expected one ActionInstalled, got %+v", rep.Actions)
	}

	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)
	body, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if !strings.Contains(string(body), "x-semantica-cli-version: v0.3.9") {
		t.Errorf("installed file not stamped with cli version:\n%s", body)
	}
	ok, vErr := Verify(body)
	if vErr != nil || !ok {
		t.Errorf("freshly installed file failed Verify: ok=%v err=%v", ok, vErr)
	}
}

func TestInstall_RerunIsIdempotentForUnchangedSourceAndVersion(t *testing.T) {
	src, dst := installTestSetup(t)
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)

	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first, _ := os.ReadFile(dstFile)

	rep, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if rep.Actions[0].Action != ActionUpdated {
		t.Errorf("expected ActionUpdated on rerun, got %v", rep.Actions[0].Action)
	}
	second, _ := os.ReadFile(dstFile)
	if string(first) != string(second) {
		t.Errorf("idempotent rerun produced different bytes\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestInstall_RefusesEditedDestinationWithoutForce(t *testing.T) {
	src, dst := installTestSetup(t)
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)

	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// Tamper with the body after install.
	body, _ := os.ReadFile(dstFile)
	tampered := strings.Replace(string(body),
		"Body content that the hash must cover.",
		"Body content edited by user.", 1)
	if err := os.WriteFile(dstFile, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := rep.Actions[0].Action; got != ActionSkipped {
		t.Errorf("expected ActionSkipped, got %v", got)
	}
	if !strings.Contains(rep.Actions[0].Reason, "edited") {
		t.Errorf("skip reason should mention edits: %q", rep.Actions[0].Reason)
	}
	// Confirm tamper is preserved.
	preserved, _ := os.ReadFile(dstFile)
	if !strings.Contains(string(preserved), "edited by user") {
		t.Errorf("tamper was overwritten despite no --force:\n%s", preserved)
	}
}

func TestInstall_ForceOverwritesEditedDestination(t *testing.T) {
	src, dst := installTestSetup(t)
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)

	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	body, _ := os.ReadFile(dstFile)
	tampered := strings.Replace(string(body),
		"Body content that the hash must cover.",
		"Body content edited by user.", 1)
	_ = os.WriteFile(dstFile, []byte(tampered), 0o644)

	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9", Force: true}); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	out, _ := os.ReadFile(dstFile)
	if strings.Contains(string(out), "edited by user") {
		t.Errorf("--force did not overwrite tampered content:\n%s", out)
	}
	ok, vErr := Verify(out)
	if vErr != nil || !ok {
		t.Errorf("forced install left a non-verifying file: ok=%v err=%v", ok, vErr)
	}
}

func TestInstall_RefusesUnmanagedDestinationWithoutForce(t *testing.T) {
	src, dst := installTestSetup(t)
	dstDir := filepath.Join(dst, "semantica-handoff")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate destination with a SKILL.md the user wrote
	// themselves (no Semantica marker).
	hostile := "# user's own SKILL.md\n"
	if err := os.WriteFile(filepath.Join(dstDir, SkillFileName), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := rep.Actions[0].Action; got != ActionSkipped {
		t.Errorf("expected ActionSkipped, got %v", got)
	}
	if !strings.Contains(rep.Actions[0].Reason, "not Semantica-managed") {
		t.Errorf("skip reason should call out the unmanaged file: %q", rep.Actions[0].Reason)
	}
	preserved, _ := os.ReadFile(filepath.Join(dstDir, SkillFileName))
	if string(preserved) != hostile {
		t.Errorf("unmanaged file was overwritten without --force:\n%s", preserved)
	}
}

func TestInstall_RejectsSourceWithDirNameMismatch(t *testing.T) {
	srcRoot := filepath.Join(t.TempDir(), "src")
	dstRoot := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	// Directory name does not match frontmatter `name`.
	skillDir := filepath.Join(srcRoot, "wrong-dirname")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(sampleSkill), 0o644)

	_, err := Install(InstallOptions{Source: srcRoot, CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSkillNameMismatch) {
		t.Errorf("expected ErrSkillNameMismatch, got %v", err)
	}
}

func TestInstall_RejectsUnsafeDirectoryName(t *testing.T) {
	srcRoot := filepath.Join(t.TempDir(), "src")
	dstRoot := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	// Path traversal attempt as the skill directory name.
	skillDir := filepath.Join(srcRoot, "..bad")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(sampleSkill), 0o644)

	_, err := Install(InstallOptions{Source: srcRoot, CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrUnsafeSkillName) && !errors.Is(err, ErrSkillNameMismatch) {
		t.Errorf("expected unsafe-name or name-mismatch error, got %v", err)
	}
}

func TestInstall_RejectsEmptySource(t *testing.T) {
	srcRoot := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	_, err := Install(InstallOptions{Source: srcRoot, CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSourceNoSkills) {
		t.Errorf("expected ErrSourceNoSkills, got %v", err)
	}
}

func TestInstall_RejectsMissingSource(t *testing.T) {
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))
	_, err := Install(InstallOptions{Source: "/nonexistent-skills-dir-12345", CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSourceMissing) {
		t.Errorf("expected ErrSourceMissing, got %v", err)
	}
}

func TestUninstall_RemovesManagedFiles(t *testing.T) {
	src, dst := installTestSetup(t)
	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)
	if _, err := os.Stat(dstFile); err != nil {
		t.Fatalf("expected file to exist before uninstall: %v", err)
	}

	rep, err := Uninstall(false)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Action != ActionRemoved {
		t.Fatalf("expected one ActionRemoved, got %+v", rep.Actions)
	}
	if _, err := os.Stat(dstFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected file to be gone after uninstall, stat err=%v", err)
	}
}

func TestUninstall_SkipsEditedFilesWithoutForce(t *testing.T) {
	src, dst := installTestSetup(t)
	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)
	body, _ := os.ReadFile(dstFile)
	tampered := strings.Replace(string(body), "Body content", "Body edited", 1)
	_ = os.WriteFile(dstFile, []byte(tampered), 0o644)

	rep, err := Uninstall(false)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if rep.Actions[0].Action != ActionSkipped {
		t.Errorf("expected ActionSkipped, got %v", rep.Actions[0].Action)
	}
	if _, err := os.Stat(dstFile); err != nil {
		t.Errorf("edited file should be preserved without --force: %v", err)
	}
}

func TestUninstall_ForceRemovesEdited(t *testing.T) {
	src, dst := installTestSetup(t)
	if _, err := Install(InstallOptions{Source: src, CLIVersion: "v0.3.9"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	dstFile := filepath.Join(dst, "semantica-handoff", SkillFileName)
	body, _ := os.ReadFile(dstFile)
	tampered := strings.Replace(string(body), "Body content", "Body edited", 1)
	_ = os.WriteFile(dstFile, []byte(tampered), 0o644)

	rep, err := Uninstall(true)
	if err != nil {
		t.Fatalf("forced Uninstall: %v", err)
	}
	if rep.Actions[0].Action != ActionForced {
		t.Errorf("expected ActionForced, got %v", rep.Actions[0].Action)
	}
	if _, err := os.Stat(dstFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("forced uninstall should remove edited file, stat err=%v", err)
	}
}

func TestUninstall_PreservesUnmanagedFiles(t *testing.T) {
	dstRoot := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	// User-written SKILL.md inside a Semantica-shaped directory name.
	dir := filepath.Join(dstRoot, "semantica-handoff")
	_ = os.MkdirAll(dir, 0o755)
	hostile := "# user's own\n"
	_ = os.WriteFile(filepath.Join(dir, SkillFileName), []byte(hostile), 0o644)

	rep, err := Uninstall(false)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if rep.Actions[0].Action != ActionSkipped {
		t.Errorf("expected ActionSkipped for unmanaged file, got %v", rep.Actions[0].Action)
	}
	preserved, _ := os.ReadFile(filepath.Join(dir, SkillFileName))
	if string(preserved) != hostile {
		t.Errorf("unmanaged file was modified:\n%s", preserved)
	}
}

// TestUninstall_ForceLeavesUnrelatedUserSkills guards the
// forced-uninstall scope: uninstall is limited to
// SemanticaSkillNamePrefix, never the entire user-global skills root.
// A user with their own `review/SKILL.md` and `handoff/SKILL.md` keeps
// both files even when --force is passed.
func TestUninstall_ForceLeavesUnrelatedUserSkills(t *testing.T) {
	dstRoot := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	// Two user-authored skills that exist in the agent's
	// user-global directory but have nothing to do with Semantica.
	plant := func(name, body string) string {
		dir := filepath.Join(dstRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, SkillFileName)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	reviewBody := "# user's own review skill\n"
	handoffBody := "# user's own handoff skill (different design)\n"
	reviewPath := plant("review", reviewBody)
	handoffPath := plant("handoff", handoffBody)

	rep, err := Uninstall(true)
	if err != nil {
		t.Fatalf("forced Uninstall: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Errorf("forced uninstall should report zero actions on a non-Semantica tree, got %+v",
			rep.Actions)
	}

	for _, p := range []struct {
		path, want string
	}{
		{reviewPath, reviewBody},
		{handoffPath, handoffBody},
	} {
		got, err := os.ReadFile(p.path)
		if err != nil {
			t.Errorf("forced uninstall deleted unrelated user skill at %s: %v", p.path, err)
			continue
		}
		if string(got) != p.want {
			t.Errorf("forced uninstall modified user skill at %s\nwant: %q\ngot:  %q",
				p.path, p.want, string(got))
		}
	}
}

// TestUninstall_ForceLeavesUnmanagedFilesInPrefixScope confirms the
// stricter invariant: even within the Semantica prefix scope, a
// SKILL.md missing the management marker is preserved under
// --force. Force only overrides the hash-mismatch case.
func TestUninstall_ForceLeavesUnmanagedFilesInPrefixScope(t *testing.T) {
	dstRoot := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dstRoot)

	dir := filepath.Join(dstRoot, "semantica-foo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := "# user authored under our prefix; no marker\n"
	path := filepath.Join(dir, SkillFileName)
	if err := os.WriteFile(path, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Uninstall(true)
	if err != nil {
		t.Fatalf("forced Uninstall: %v", err)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Action != ActionSkipped {
		t.Fatalf("expected single ActionSkipped, got %+v", rep.Actions)
	}
	preserved, _ := os.ReadFile(path)
	if string(preserved) != hostile {
		t.Errorf("--force deleted an unmanaged file under our prefix:\n%s", preserved)
	}
}

func TestInstall_RejectsNonPrefixedSkillName(t *testing.T) {
	srcRoot := filepath.Join(t.TempDir(), "src")
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	// SKILL.md whose frontmatter `name` does not carry the
	// Semantica prefix. Both source dir name and frontmatter name
	// match, so the only failure is the prefix check itself.
	src := strings.Replace(sampleSkill, "name: semantica-handoff", "name: handoff", 1)
	skillDir := filepath.Join(srcRoot, "handoff")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(src), 0o644)

	_, err := Install(InstallOptions{Source: srcRoot, CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSkillNameNotPrefixed) {
		t.Errorf("expected ErrSkillNameNotPrefixed, got %v", err)
	}
}

func TestUninstall_NoSkillsDirIsClean(t *testing.T) {
	// Skills dir does not exist at all (fresh machine, never installed).
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "never-existed", "skills"))
	rep, err := Uninstall(false)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Errorf("expected empty report, got %+v", rep.Actions)
	}
}
