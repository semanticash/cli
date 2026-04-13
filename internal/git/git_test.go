package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return canonical
}

// findGitRoot tests.

func TestFindGitRoot_NormalRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findGitRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalPath(t, dir)
	if got != want {
		t.Errorf("findGitRoot = %q, want %q", got, want)
	}
}

func TestFindGitRoot_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findGitRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalPath(t, dir)
	if got != want {
		t.Errorf("findGitRoot from subdirectory = %q, want %q", got, want)
	}
}

func TestFindGitRoot_WorktreeFile(t *testing.T) {
	// Worktrees use a .git file (not directory) pointing to the main repo.
	dir := t.TempDir()
	gitFile := filepath.Join(dir, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /some/other/path\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findGitRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalPath(t, dir)
	if got != want {
		t.Errorf("findGitRoot with .git file = %q, want %q", got, want)
	}
}

func TestFindGitRoot_NotARepo(t *testing.T) {
	dir := t.TempDir()
	_, err := findGitRoot(dir)
	if err == nil {
		t.Fatal("expected error for non-repo directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unexpected error: %v", err)
	}
}

// OpenRepo tests.

func TestOpenRepo_ValidRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalPath(t, dir)
	if r.Root() != want {
		t.Errorf("Root() = %q, want %q", r.Root(), want)
	}
}

func TestOpenRepo_NotARepo(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenRepo(dir)
	if err == nil {
		t.Fatal("expected error for non-repo directory")
	}
}

func TestOpenRepo_CanonicalizesSymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "repo-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	r, err := OpenRepo(link)
	if err != nil {
		t.Fatal(err)
	}
	want := canonicalPath(t, dir)
	if r.Root() != want {
		t.Errorf("Root() = %q, want canonical root %q", r.Root(), want)
	}
}

// RestoreFile tests.

func TestRestoreFile_RegularFile(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	if err := r.RestoreFile("test.txt", []byte("hello\n"), 0o644, false, ""); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("content = %q, want %q", got, "hello\n")
	}
}

func TestRestoreFile_ExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable permission bits are not supported on Windows")
	}
	dir := t.TempDir()
	r := &Repo{root: dir}

	if err := r.RestoreFile("script.sh", []byte("#!/bin/sh\n"), 0o755, false, ""); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dir, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("script.sh not executable: mode = %o", fi.Mode().Perm())
	}
}

func TestRestoreFile_ZeroModeFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not supported on Windows")
	}
	dir := t.TempDir()
	r := &Repo{root: dir}

	// Mode 0 should default to 0644 for backward compat.
	if err := r.RestoreFile("old.txt", []byte("old\n"), 0, false, ""); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dir, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestRestoreFile_Symlink(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	// Create target first.
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.RestoreFile("link.txt", nil, 0, true, "target.txt"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link.txt is not a symlink")
	}

	target, err := os.Readlink(filepath.Join(dir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "target.txt" {
		t.Errorf("symlink target = %q, want %q", target, "target.txt")
	}
}

func TestRestoreFile_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	// Write initial file.
	abs := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(abs, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Restore should overwrite.
	if err := r.RestoreFile("f.txt", []byte("new\n"), 0o644, false, ""); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Errorf("content = %q, want %q", got, "new\n")
	}
}

func TestRestoreFile_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	if err := r.RestoreFile("a/b/c/deep.txt", []byte("deep\n"), 0o644, false, ""); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a", "b", "c", "deep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "deep\n" {
		t.Errorf("content = %q, want %q", got, "deep\n")
	}
}

// RemoveFile tests.

func TestRemoveFile_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	abs := filepath.Join(dir, "delete-me.txt")
	if err := os.WriteFile(abs, []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.RemoveFile("delete-me.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestRemoveFile_MissingFile(t *testing.T) {
	dir := t.TempDir()
	r := &Repo{root: dir}

	// Removing a nonexistent file should not error.
	if err := r.RemoveFile("does-not-exist.txt"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Hook script generation tests.

func TestBuildSemanticaHookScript_PreCommit(t *testing.T) {
	script := string(buildSemanticaHookScript("pre-commit", "pre-commit", false))

	if !strings.Contains(script, "#!/bin/sh") {
		t.Error("missing shebang")
	}
	if !strings.Contains(script, semanticaHookMarker) {
		t.Error("missing hook marker")
	}
	if !strings.Contains(script, `[ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0`) {
		t.Error("missing marker file gate")
	}
	if !strings.Contains(script, "semantica hook pre-commit") {
		t.Error("missing hook subcommand")
	}
	// No args when PassArgs is false.
	if strings.Contains(script, `"$@"`) {
		t.Error("should not pass args when PassArgs=false")
	}
	if !strings.Contains(script, ">/dev/null 2>&1") {
		t.Error("pre-commit hook should suppress output")
	}
}

func TestBuildSemanticaHookScript_CommitMsg_PassArgs(t *testing.T) {
	script := string(buildSemanticaHookScript("commit-msg", "commit-msg", true))

	if !strings.Contains(script, `"$1"`) {
		t.Error("commit-msg hook should pass $1")
	}
	if strings.Contains(script, `"$@"`) {
		t.Error("commit-msg hook should use $1, not $@")
	}
}

func TestBuildSemanticaHookScript_PostCommit_PassArgs(t *testing.T) {
	script := string(buildSemanticaHookScript("post-commit", "post-commit", true))

	if !strings.Contains(script, `"$@"`) {
		t.Error("post-commit hook should pass $@")
	}
	if strings.Contains(script, `semantica hook post-commit "$@" >/dev/null 2>&1`) {
		t.Error("post-commit hook should keep output visible")
	}
}

func TestBuildSemanticaHookWrapperScript(t *testing.T) {
	script := string(buildSemanticaHookWrapperScript("pre-commit", "pre-commit.user.123", "pre-commit", false))

	if !strings.Contains(script, "#!/bin/sh") {
		t.Error("missing shebang")
	}
	if !strings.Contains(script, semanticaHookMarker) {
		t.Error("missing hook marker")
	}
	if !strings.Contains(script, "pre-commit.user.123") {
		t.Error("missing user hook reference")
	}
	if !strings.Contains(script, `[ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0`) {
		t.Error("missing marker file gate")
	}
}

func TestBuildSemanticaHookWrapperScript_CommitMsg_PassArgs(t *testing.T) {
	script := string(buildSemanticaHookWrapperScript("commit-msg", "commit-msg.user.100", "commit-msg", true))

	// commit-msg wrapper should pass "$1" (the message file path), not "$@".
	if !strings.Contains(script, `semantica hook commit-msg "$1"`) {
		t.Error("commit-msg wrapper should forward $1 to semantica hook")
	}
	if strings.Contains(script, `semantica hook commit-msg "$@"`) {
		t.Error("commit-msg wrapper should use $1, not $@")
	}

	// User hook should still receive "$@" (the wrapper always passes "$@" to user hooks).
	userExecIdx := strings.Index(script, `commit-msg.user.100" "$@"`)
	if userExecIdx < 0 {
		t.Error("user hook should receive $@ in wrapper")
	}
}

func TestBuildSemanticaHookWrapperScript_PostCommit_VisibleOutput(t *testing.T) {
	script := string(buildSemanticaHookWrapperScript("post-commit", "post-commit.user.100", "post-commit", true))

	if strings.Contains(script, `semantica hook post-commit "$@" >/dev/null 2>&1`) {
		t.Error("post-commit wrapper should not suppress semantica output")
	}
}

func TestBuildSemanticaHookWrapperScript_RunsUserHookFirst(t *testing.T) {
	script := string(buildSemanticaHookWrapperScript("pre-commit", "pre-commit.user.999", "pre-commit", false))

	// User hook execution should come before semantica's.
	userIdx := strings.Index(script, "pre-commit.user.999")
	semIdx := strings.Index(script, "semantica hook pre-commit")

	if userIdx < 0 {
		t.Fatal("user hook not found in wrapper")
	}
	if semIdx < 0 {
		t.Fatal("semantica hook not found in wrapper")
	}
	if userIdx > semIdx {
		t.Error("user hook should execute before semantica hook")
	}
}
