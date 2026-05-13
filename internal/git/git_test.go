package git

import (
	"context"
	"os"
	"os/exec"
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

// TestBuildSemanticaHookWrapperScript_PropagatesUserHookExitCode
// verifies that preserved user hooks keep normal Git blocking
// semantics while Semantica's capture hook remains non-blocking.
func TestBuildSemanticaHookWrapperScript_PropagatesUserHookExitCode(t *testing.T) {
	script := string(buildSemanticaHookWrapperScript("pre-commit", "pre-commit.user.42", "pre-commit", false))

	// Assert on the shell pattern instead of executing a shell so
	// the test stays portable.
	if !strings.Contains(script, "user_rc=$?") {
		t.Errorf("user hook exit code must be captured; got:\n%s", script)
	}
	if !strings.Contains(script, "if [ $user_rc -ne 0 ]; then") {
		t.Errorf("user hook exit code must be checked; got:\n%s", script)
	}
	if !strings.Contains(script, "exit $user_rc") {
		t.Errorf("non-zero user-hook exit must propagate; got:\n%s", script)
	}

	// The user-hook line must not swallow failures with `|| true`.
	if strings.Contains(script, `"$HOOK_DIR/pre-commit.user.42" "$@" || true`) {
		t.Errorf("user hook line must not swallow failures via || true; got:\n%s", script)
	}

	// Semantica's own hook remains non-blocking.
	if !strings.Contains(script, "semantica hook pre-commit") {
		t.Fatalf("script missing semantica hook line:\n%s", script)
	}
	semIdx := strings.Index(script, "semantica hook pre-commit")
	tail := script[semIdx:]
	if !strings.Contains(tail, "|| true") {
		t.Errorf("semantica hook line should retain `|| true` (non-blocking capture); got tail:\n%s", tail)
	}
}

// TestInstallSemanticaHook_ReinstallPreservesWrapper verifies that
// reinstalling over a Semantica wrapper keeps the preserved user hook
// in the execution chain instead of replacing the wrapper with the
// plain Semantica script.
//
// This test simulates the full sequence:
//  1. Pre-existing user hook installed by team policy.
//  2. First `semantica enable` wraps it (rename + wrapper write).
//  3. Second `semantica enable` (reinstall / upgrade).
//  4. The resulting hook still references the preserved file.
func TestInstallSemanticaHook_ReinstallPreservesWrapper(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	ctx := context.Background()
	hooksDir, err := repo.HooksDir(ctx)
	if err != nil {
		t.Fatalf("HooksDir: %v", err)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Pre-existing user hook: a fake lint/policy gate.
	hookPath := filepath.Join(hooksDir, "pre-commit")
	const userHookBody = "#!/bin/sh\n# user policy hook\nexit 1\n"
	if err := os.WriteFile(hookPath, []byte(userHookBody), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := HookInstallOptions{Name: "pre-commit", Subcommand: "pre-commit", PassArgs: false}

	// 2. First install: should wrap the user hook.
	if err := repo.InstallSemanticaHook(ctx, opts); err != nil {
		t.Fatalf("first install: %v", err)
	}
	afterFirst, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read after first install: %v", err)
	}
	if !strings.Contains(string(afterFirst), "# Preserved user hook:") {
		t.Fatalf("first install should produce a wrapper:\n%s", afterFirst)
	}
	preservedName := parsePreservedUserHook(afterFirst)
	if preservedName == "" {
		t.Fatalf("first install wrapper missing preserved-user-hook marker")
	}
	// The preserved file must exist on disk.
	if _, err := os.Stat(filepath.Join(hooksDir, preservedName)); err != nil {
		t.Fatalf("preserved user hook file missing: %v", err)
	}

	// 3. Second install: regenerate as wrapper, not plain.
	if err := repo.InstallSemanticaHook(ctx, opts); err != nil {
		t.Fatalf("second install: %v", err)
	}
	afterSecond, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read after second install: %v", err)
	}

	// 4. Assertions: wrapper preserved, same user-hook file
	// referenced, user hook still on disk.
	if !strings.Contains(string(afterSecond), "# Preserved user hook:") {
		t.Errorf("reinstall must regenerate as wrapper (not plain); got:\n%s", afterSecond)
	}
	if got := parsePreservedUserHook(afterSecond); got != preservedName {
		t.Errorf("reinstall must reference the same user hook %q; got %q\nscript:\n%s",
			preservedName, got, afterSecond)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, preservedName)); err != nil {
		t.Errorf("preserved user hook file should still exist after reinstall: %v", err)
	}
	// The plain script also contains "semantica hook pre-commit";
	// the wrapper is identified by the marker and user-hook call.
	if !strings.Contains(string(afterSecond), "$HOOK_DIR/"+preservedName) {
		t.Errorf("reinstall wrapper should still invoke %s; got:\n%s",
			preservedName, afterSecond)
	}
}

// TestIsValidPreservedHookName covers the filename validator used
// before a preserved-hook reference is written back into a wrapper.
func TestIsValidPreservedHookName(t *testing.T) {
	cases := []struct {
		name     string
		hookName string
		input    string
		want     bool
	}{
		// Accepts the exact generated shape.
		{"valid pre-commit", "pre-commit", "pre-commit.user.1715526789123", true},
		{"valid commit-msg", "commit-msg", "commit-msg.user.42", true},
		{"valid post-commit", "post-commit", "post-commit.user.0", true},

		// Cross-hook references rejected.
		{"cross-hook prefix", "pre-commit", "commit-msg.user.42", false},
		{"hook substring not prefix", "pre-commit", "xpre-commit.user.42", false},

		// Shape rejects.
		{"missing timestamp", "pre-commit", "pre-commit.user.", false},
		{"non-numeric timestamp", "pre-commit", "pre-commit.user.abc", false},
		{"missing .user. segment", "pre-commit", "pre-commit.42", false},
		{"trailing junk", "pre-commit", "pre-commit.user.42x", false},

		// Adversarial: path traversal and separators.
		{"parent traversal", "pre-commit", "../pre-commit.user.42", false},
		{"path separator", "pre-commit", "subdir/pre-commit.user.42", false},
		{"absolute path", "pre-commit", "/etc/passwd", false},
		{"backslash separator", "pre-commit", "subdir\\pre-commit.user.42", false},

		// Adversarial: shell injection attempts.
		{"command injection via semicolon", "pre-commit", "pre-commit.user.42;rm", false},
		{"command substitution", "pre-commit", "pre-commit.user.$(id)", false},
		{"double quote", "pre-commit", `pre-commit.user.42"`, false},
		{"single quote", "pre-commit", "pre-commit.user.42'", false},
		{"backtick", "pre-commit", "pre-commit.user.`whoami`", false},

		// Whitespace rejects.
		{"leading space", "pre-commit", " pre-commit.user.42", false},
		{"trailing space", "pre-commit", "pre-commit.user.42 ", false},
		{"embedded space", "pre-commit", "pre-commit.user.4 2", false},
		{"newline", "pre-commit", "pre-commit.user.42\n", false},

		// Empty / pathological.
		{"empty", "pre-commit", "", false},
		{"just dot", "pre-commit", ".", false},
		{"just user.", "pre-commit", ".user.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isValidPreservedHookName(tc.input, tc.hookName)
			if got != tc.want {
				t.Errorf("isValidPreservedHookName(%q, %q) = %v, want %v",
					tc.input, tc.hookName, got, tc.want)
			}
		})
	}
}

// TestInstallSemanticaHook_DamagedWrapperRefusesInstall verifies
// that damaged wrapper metadata produces a clear error instead of
// being reused to generate a new wrapper.
func TestInstallSemanticaHook_DamagedWrapperRefusesInstall(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	ctx := context.Background()
	hooksDir, err := repo.HooksDir(ctx)
	if err != nil {
		t.Fatalf("HooksDir: %v", err)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "pre-commit")
	opts := HookInstallOptions{Name: "pre-commit", Subcommand: "pre-commit", PassArgs: false}

	// Each wrapper carries the Semantica marker but has an invalid
	// preserved-hook reference.
	cases := []struct {
		name      string
		corrupted string
	}{
		{
			name: "path traversal",
			corrupted: "#!/bin/sh\n# " + semanticaHookMarker + " (wrapper): pre-commit\n" +
				"# Preserved user hook: ../etc/passwd\n",
		},
		{
			name: "shell injection",
			corrupted: "#!/bin/sh\n# " + semanticaHookMarker + " (wrapper): pre-commit\n" +
				"# Preserved user hook: foo\"; rm -rf $HOME; \"\n",
		},
		{
			name: "cross-hook drift",
			corrupted: "#!/bin/sh\n# " + semanticaHookMarker + " (wrapper): pre-commit\n" +
				"# Preserved user hook: commit-msg.user.42\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(hookPath, []byte(tc.corrupted), 0o755); err != nil {
				t.Fatal(err)
			}
			err := repo.InstallSemanticaHook(ctx, opts)
			if err == nil {
				t.Fatalf("expected install error for damaged wrapper; hook was rewritten")
			}
			if !strings.Contains(err.Error(), "damaged or hand-edited") {
				t.Errorf("error should explain the failure; got %v", err)
			}
			// Refusing to install must leave the damaged file untouched.
			after, readErr := os.ReadFile(hookPath)
			if readErr != nil {
				t.Fatalf("read after refused install: %v", readErr)
			}
			if string(after) != tc.corrupted {
				t.Errorf("damaged file was modified despite install error; before=%d bytes after=%d bytes",
					len(tc.corrupted), len(after))
			}
		})
	}
}

// TestParsePreservedUserHook covers the three relevant inputs to
// the wrapper-detection logic that drives the reinstall fix.
func TestParsePreservedUserHook(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrapper with marker",
			in:   "#!/bin/sh\n# Semantica git hook (wrapper): pre-commit\n# Preserved user hook: pre-commit.user.42\n",
			want: "pre-commit.user.42",
		},
		{
			name: "wrapper with trailing whitespace",
			in:   "# Preserved user hook:   commit-msg.user.99  \n",
			want: "commit-msg.user.99",
		},
		{
			name: "plain Semantica hook (no wrapper)",
			in:   "#!/bin/sh\n# Semantica git hook: pre-commit\n# Semantica git hook\n",
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "marker with empty filename",
			in:   "# Preserved user hook: \n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePreservedUserHook([]byte(tc.in))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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
