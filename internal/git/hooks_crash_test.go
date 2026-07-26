package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const userHookBody = "#!/bin/sh\n# user policy hook\nexit 1\n"

var installOpts = HookInstallOptions{Name: "pre-commit", Subcommand: "pre-commit"}

func newHookTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()
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
	hooksDir, err := repo.HooksDir(context.Background())
	if err != nil {
		t.Fatalf("HooksDir: %v", err)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, hooksDir
}

// writeUserHook seeds a pre-existing non-Semantica hook.
func writeUserHook(t *testing.T, hooksDir string) string {
	t.Helper()
	hookPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte(userHookBody), 0o755); err != nil {
		t.Fatal(err)
	}
	return hookPath
}

// assertOriginalIntact verifies the user hook is still the active hook
// and no temp or backup files leaked into the hooks directory.
func assertOriginalIntact(t *testing.T, hooksDir, hookPath string) {
	t.Helper()
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook after failed install: %v", err)
	}
	if string(got) != userHookBody {
		t.Errorf("hook content changed after failed install:\n%s", got)
	}
	for _, pattern := range []string{".semantica-hook-*", "pre-commit.user.*"} {
		matches, err := filepath.Glob(filepath.Join(hooksDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Errorf("leftover %s files after failed install: %v", pattern, matches)
		}
	}
}

func TestInstallSemanticaHook_TempCreateFailureLeavesHookActive(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookCreateTemp
	hookCreateTemp = func(dir, pattern string) (*os.File, error) {
		return nil, errors.New("injected temp create failure")
	}
	t.Cleanup(func() { hookCreateTemp = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail when temp creation fails")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_TempWriteFailureLeavesHookActive(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookTempWrite
	hookTempWrite = func(f *os.File, b []byte) (int, error) {
		return 0, errors.New("injected temp write failure")
	}
	t.Cleanup(func() { hookTempWrite = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail when the temp write fails")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_ChmodFailureLeavesHookActive(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookChmod
	hookChmod = func(name string, mode os.FileMode) error {
		return errors.New("injected chmod failure")
	}
	t.Cleanup(func() { hookChmod = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail when chmod fails")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_BackupCreateFailureLeavesHookActive(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookCreateExcl
	hookCreateExcl = func(path string) (*os.File, error) {
		return nil, errors.New("injected backup create failure")
	}
	t.Cleanup(func() { hookCreateExcl = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail when backup creation fails")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_BackupValidationFailureLeavesHookActive(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookReadFile
	hookReadFile = func(name string) ([]byte, error) {
		return []byte("corrupted"), nil
	}
	t.Cleanup(func() { hookReadFile = orig })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("install should fail when backup validation fails")
	}
	if !strings.Contains(err.Error(), "validate hook backup") {
		t.Errorf("error should name backup validation: %v", err)
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_PromotionFailureKeepsOldHook(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail when promotion fails")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_PartialPromotionRestoresOriginal(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		// Simulate a failed replacement that leaves the destination missing.
		_ = os.Remove(dst)
		return errors.New("injected partial promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("install should fail on partial promotion")
	}
	assertOriginalIntact(t, hooksDir, hookPath)
}

func TestInstallSemanticaHook_PartialPromotionRestoreFailureReportsBackup(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)

	origRename := hookRename
	hookRename = func(src, dst string) error {
		_ = os.Remove(dst)
		return errors.New("injected partial promotion failure")
	}
	t.Cleanup(func() { hookRename = origRename })

	origWrite := hookWriteFile
	hookWriteFile = func(name string, data []byte, perm os.FileMode) error {
		return errors.New("injected restore failure")
	}
	t.Cleanup(func() { hookWriteFile = origWrite })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("install should fail on partial promotion with failed restore")
	}
	if !strings.Contains(err.Error(), "could not be restored") ||
		!strings.Contains(err.Error(), "pre-commit.user.") {
		t.Errorf("error must report the unrestored state and backup path: %v", err)
	}
	// The verified backup must survive for manual recovery.
	matches, globErr := filepath.Glob(filepath.Join(hooksDir, "pre-commit.user.*"))
	if globErr != nil || len(matches) != 1 {
		t.Fatalf("backup should survive failed restore: %v %v", matches, globErr)
	}
	got, readErr := os.ReadFile(matches[0])
	if readErr != nil || string(got) != userHookBody {
		t.Errorf("surviving backup content = %q, %v", got, readErr)
	}
}

func TestInstallSemanticaHook_LateReplaceErrorRetainsBackup(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		// Replacement lands, then an error is reported anyway.
		if err := orig(src, dst); err != nil {
			return err
		}
		return errors.New("injected late replace error")
	}
	t.Cleanup(func() { hookRename = orig })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("install should surface the late replace error")
	}
	if !strings.Contains(err.Error(), "backup retained") {
		t.Errorf("error should report the retained backup: %v", err)
	}
	// The wrapper is active; the backup it references must survive.
	backupPath := assertWrapperInstalled(t, hooksDir)
	got, readErr := os.ReadFile(backupPath)
	if readErr != nil || string(got) != userHookBody {
		t.Errorf("referenced backup = %q, %v", got, readErr)
	}
}

func TestInstallSemanticaHook_UnrecognizedDestinationNotOverwritten(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	hookPath := writeUserHook(t, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		// The destination ends up as something we did not write.
		if err := os.WriteFile(dst, []byte("external content"), 0o755); err != nil {
			return err
		}
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("install should fail")
	}
	if !strings.Contains(err.Error(), "unrecognized content") {
		t.Errorf("error should report unrecognized content: %v", err)
	}
	// The unrecognized hook must not be overwritten.
	got, readErr := os.ReadFile(hookPath)
	if readErr != nil || string(got) != "external content" {
		t.Errorf("unrecognized hook was modified: %q, %v", got, readErr)
	}
	// The verified backup survives for manual recovery.
	matches, globErr := filepath.Glob(filepath.Join(hooksDir, "pre-commit.user.*"))
	if globErr != nil || len(matches) != 1 {
		t.Fatalf("backup should survive: %v %v", matches, globErr)
	}
}

func TestReinstall_UnrecognizedDestinationNotOverwritten(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)
	_, preserved := installWrapper(t, r, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		if err := os.WriteFile(dst, []byte("external content"), 0o755); err != nil {
			return err
		}
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("reinstall should fail")
	}
	if !strings.Contains(err.Error(), "unrecognized content") {
		t.Errorf("error should report unrecognized content: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(hooksDir, "pre-commit"))
	if readErr != nil || string(got) != "external content" {
		t.Errorf("unrecognized hook was modified: %q, %v", got, readErr)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, preserved)); err != nil {
		t.Errorf("preserved user hook must survive: %v", err)
	}
}

func TestInstallSemanticaHook_BackupNameCollisionRetries(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)

	calls := 0
	orig := hookCreateExcl
	hookCreateExcl = func(path string) (*os.File, error) {
		calls++
		if calls == 1 {
			return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrExist}
		}
		return orig(path)
	}
	t.Cleanup(func() { hookCreateExcl = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err != nil {
		t.Fatalf("install should retry past a name collision: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 backup create attempts, got %d", calls)
	}
	assertWrapperInstalled(t, hooksDir)
}

func TestInstallSemanticaHook_BackupPreservesContentAndMode(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err != nil {
		t.Fatalf("install: %v", err)
	}
	backupPath := assertWrapperInstalled(t, hooksDir)
	got, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != userHookBody {
		t.Errorf("backup content = %q, want original", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(backupPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("backup mode = %v, want 0755", st.Mode().Perm())
		}
	}
}

func TestInstallSemanticaHook_SymlinkHookPreservedAsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privileges on Windows")
	}
	r, hooksDir := newHookTestRepo(t)
	target := filepath.Join(hooksDir, "team-hook.sh")
	if err := os.WriteFile(target, []byte(userHookBody), 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.Symlink(target, hookPath); err != nil {
		t.Fatal(err)
	}

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err != nil {
		t.Fatalf("install over symlink hook: %v", err)
	}
	backupPath := assertWrapperInstalled(t, hooksDir)
	linkTarget, err := os.Readlink(backupPath)
	if err != nil {
		t.Fatalf("backup should be a symlink: %v", err)
	}
	if linkTarget != target {
		t.Errorf("backup symlink target = %q, want %q", linkTarget, target)
	}
}

func TestInstallSemanticaHook_SymlinkToSemanticaHookRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privileges on Windows")
	}
	r, hooksDir := newHookTestRepo(t)
	target := filepath.Join(hooksDir, "elsewhere")
	if err := os.WriteFile(target, buildSemanticaHookScript("pre-commit", "pre-commit", false), 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.Symlink(target, hookPath); err != nil {
		t.Fatal(err)
	}

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("symlink to a Semantica hook should be rejected")
	}
	if _, lerr := os.Readlink(hookPath); lerr != nil {
		t.Errorf("rejected symlink hook must remain in place: %v", lerr)
	}
}

// installWrapper performs a real first install over the user hook and
// returns the active wrapper bytes and preserved backup name.
func installWrapper(t *testing.T, r *Repo, hooksDir string) ([]byte, string) {
	t.Helper()
	if err := r.InstallSemanticaHook(context.Background(), installOpts); err != nil {
		t.Fatalf("first install: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(hooksDir, "pre-commit"))
	if err != nil {
		t.Fatal(err)
	}
	name := parsePreservedUserHook(content)
	if name == "" {
		t.Fatalf("first install did not produce a wrapper:\n%s", content)
	}
	return content, name
}

func TestReinstall_PromotionFailureKeepsWrapper(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)
	wrapper, preserved := installWrapper(t, r, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		return errors.New("injected promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("reinstall should fail when promotion fails")
	}
	got, err := os.ReadFile(filepath.Join(hooksDir, "pre-commit"))
	if err != nil || string(got) != string(wrapper) {
		t.Errorf("failed reinstall must leave the previous wrapper active: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, preserved)); err != nil {
		t.Errorf("preserved user hook must survive: %v", err)
	}
}

func TestReinstall_PartialPromotionRestoresWrapper(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)
	wrapper, preserved := installWrapper(t, r, hooksDir)

	orig := hookRename
	hookRename = func(src, dst string) error {
		_ = os.Remove(dst)
		return errors.New("injected partial promotion failure")
	}
	t.Cleanup(func() { hookRename = orig })

	if err := r.InstallSemanticaHook(context.Background(), installOpts); err == nil {
		t.Fatal("reinstall should fail on partial promotion")
	}
	got, err := os.ReadFile(filepath.Join(hooksDir, "pre-commit"))
	if err != nil {
		t.Fatalf("hook missing after restore: %v", err)
	}
	if string(got) != string(wrapper) {
		t.Errorf("restored hook differs from previous wrapper:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, preserved)); err != nil {
		t.Errorf("preserved user hook must survive: %v", err)
	}
}

func TestReinstall_PartialPromotionRestoreFailureReports(t *testing.T) {
	r, hooksDir := newHookTestRepo(t)
	writeUserHook(t, hooksDir)
	_, preserved := installWrapper(t, r, hooksDir)

	origRename := hookRename
	hookRename = func(src, dst string) error {
		_ = os.Remove(dst)
		return errors.New("injected partial promotion failure")
	}
	t.Cleanup(func() { hookRename = origRename })

	origWrite := hookWriteFile
	hookWriteFile = func(name string, data []byte, perm os.FileMode) error {
		return errors.New("injected restore failure")
	}
	t.Cleanup(func() { hookWriteFile = origWrite })

	err := r.InstallSemanticaHook(context.Background(), installOpts)
	if err == nil {
		t.Fatal("reinstall should fail on partial promotion with failed restore")
	}
	if !strings.Contains(err.Error(), "could not be restored") {
		t.Errorf("error must report the unrestored state: %v", err)
	}
	// The preserved user hook remains available for manual recovery.
	got, readErr := os.ReadFile(filepath.Join(hooksDir, preserved))
	if readErr != nil || string(got) != userHookBody {
		t.Errorf("preserved user hook must survive: %q, %v", got, readErr)
	}
}

// assertWrapperInstalled verifies the active hook is a wrapper whose
// preserved backup exists, and returns the backup path.
func assertWrapperInstalled(t *testing.T, hooksDir string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(hooksDir, "pre-commit"))
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}
	name := parsePreservedUserHook(content)
	if name == "" {
		t.Fatalf("installed hook is not a wrapper:\n%s", content)
	}
	if !isValidPreservedHookName(name, "pre-commit") {
		t.Fatalf("preserved name %q has invalid shape", name)
	}
	backupPath := filepath.Join(hooksDir, name)
	if _, err := os.Lstat(backupPath); err != nil {
		t.Fatalf("preserved backup missing: %v", err)
	}
	return backupPath
}
