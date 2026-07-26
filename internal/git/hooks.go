package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

const semanticaHookMarker = "Semantica git hook"

// SemanticaHookMarker returns the marker written into Semantica-owned git hooks.
func SemanticaHookMarker() string { return semanticaHookMarker }

type HookInstallOptions struct {
	Name       string // "pre-commit", "post-commit"
	Subcommand string // "pre-commit", "post-commit"
	PassArgs   bool   // if true, pass "$@" to semantica hook
}

func (r *Repo) HooksDir(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "rev-parse", "--git-path", "hooks")

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git rev-parse --git-path hooks failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git rev-parse --git-path hooks failed: %w", err)
	}

	rel := strings.TrimSpace(string(out))
	if rel == "" {
		return "", fmt.Errorf("git returned empty hooks path")
	}

	hooksPath := rel
	if !filepath.IsAbs(hooksPath) {
		hooksPath = filepath.Join(r.root, filepath.FromSlash(hooksPath))
	}
	return hooksPath, nil
}

// Filesystem seams overridden by fault-injection tests.
var (
	hookCreateTemp = os.CreateTemp
	hookTempWrite  = (*os.File).Write
	hookChmod      = os.Chmod
	hookRename     = platform.ReplaceFile
	hookCreateExcl = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	hookReadFile  = os.ReadFile
	hookWriteFile = os.WriteFile
	hookSymlink   = os.Symlink
)

func (r *Repo) InstallSemanticaHook(ctx context.Context, opts HookInstallOptions) error {
	if opts.Name == "" || opts.Subcommand == "" {
		return fmt.Errorf("hook opts missing Name/Subcommand")
	}

	hooksDir, err := r.HooksDir(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("mkdir hooks dir: %w", err)
	}

	hookPath := filepath.Join(hooksDir, opts.Name)
	desired := buildSemanticaHookScript(opts.Name, opts.Subcommand, opts.PassArgs)

	// If no hook exists, write ours.
	info, err := os.Lstat(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return writeExecutableFile(hookPath, desired)
		}
		return fmt.Errorf("stat existing hook %s: %w", opts.Name, err)
	}
	isSymlink := info.Mode()&os.ModeSymlink != 0

	existing, err := os.ReadFile(hookPath)
	if err != nil {
		return fmt.Errorf("read existing hook %s: %w", opts.Name, err)
	}

	// If the current hook is Semantica-managed, regenerate it. A
	// wrapper must stay a wrapper so any preserved user hook remains
	// in the execution chain across re-enable or upgrade.
	if bytes.Contains(existing, []byte(semanticaHookMarker)) {
		// Semantica installs regular files, never symlinks.
		if isSymlink {
			return fmt.Errorf("hook %s is a symlink to a Semantica hook; inspect %s manually", opts.Name, hookPath)
		}
		userHookFile := parsePreservedUserHook(existing)
		if userHookFile == "" {
			// Plain Semantica hook (no preserved wrapper). Safe to
			// regenerate as the plain form.
			return replaceHookFile(hookPath, desired, existing, info.Mode().Perm())
		}
		// The parsed filename is written back into the wrapper
		// script. Only accept the generated backup filename shape;
		// damaged or hand-edited wrappers are left untouched.
		if !isValidPreservedHookName(userHookFile, opts.Name) {
			return fmt.Errorf("hook %s appears to be a damaged or hand-edited Semantica wrapper "+
				"(preserved-hook reference %q does not match the generated shape <hook>.user.<digits>); "+
				"inspect %s manually before retrying", opts.Name, userHookFile, hookPath)
		}
		wrapper := buildSemanticaHookWrapperScript(opts.Name, userHookFile, opts.Subcommand, opts.PassArgs)
		return replaceHookFile(hookPath, wrapper, existing, info.Mode().Perm())
	}

	return wrapExistingHook(hookPath, hooksDir, info, existing, opts)
}

// wrapExistingHook installs a wrapper over a non-Semantica hook.
// Ordering: the old hook stays active until a verified backup copy
// exists, then the prepared wrapper is promoted atomically.
func wrapExistingHook(hookPath, hooksDir string, info os.FileInfo, existing []byte, opts HookInstallOptions) error {
	isSymlink := info.Mode()&os.ModeSymlink != 0
	var linkTarget string
	if isSymlink {
		t, err := os.Readlink(hookPath)
		if err != nil {
			return fmt.Errorf("readlink hook %s: %w", opts.Name, err)
		}
		linkTarget = t
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		backupName := fmt.Sprintf("%s.user.%d", opts.Name, time.Now().UnixNano())
		backupPath := filepath.Join(hooksDir, backupName)

		wrapper := buildSemanticaHookWrapperScript(opts.Name, backupName, opts.Subcommand, opts.PassArgs)
		tmpPath, err := prepareHookFile(hookPath, wrapper)
		if err != nil {
			return err
		}

		var backupErr error
		if isSymlink {
			backupErr = hookSymlink(linkTarget, backupPath)
		} else {
			backupErr = writeBackupCopy(backupPath, existing, info.Mode().Perm())
		}
		if backupErr != nil {
			_ = os.Remove(tmpPath)
			if errors.Is(backupErr, fs.ErrExist) {
				lastErr = backupErr
				continue // name taken: retry with a fresh one
			}
			return fmt.Errorf("back up hook %s: %w", opts.Name, backupErr)
		}

		if err := validateBackup(backupPath, existing, info, isSymlink, linkTarget); err != nil {
			_ = os.Remove(tmpPath)
			_ = os.Remove(backupPath)
			return fmt.Errorf("validate hook backup %s: %w", backupName, err)
		}

		return promoteWrapper(tmpPath, hookPath, backupPath, wrapper, existing, info, isSymlink, linkTarget, opts)
	}
	return fmt.Errorf("allocate unique backup name for hook %s: %w", opts.Name, lastErr)
}

// writeBackupCopy writes content to a fresh file at path (failing if
// it exists) with the original hook's permissions.
func writeBackupCopy(path string, content []byte, mode os.FileMode) error {
	f, err := hookCreateExcl(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := hookChmod(path, mode); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// validateBackup confirms the backup preserves the original hook's
// content and permissions (or symlink target) before promotion.
func validateBackup(backupPath string, content []byte, info os.FileInfo, isSymlink bool, linkTarget string) error {
	if isSymlink {
		got, err := os.Readlink(backupPath)
		if err != nil {
			return err
		}
		if got != linkTarget {
			return fmt.Errorf("backup symlink points to %q, want %q", got, linkTarget)
		}
		return nil
	}
	got, err := hookReadFile(backupPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("backup content differs from original")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(backupPath)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != info.Mode().Perm() {
			return fmt.Errorf("backup mode %v, want %v", st.Mode().Perm(), info.Mode().Perm())
		}
	}
	return nil
}

// promoteWrapper replaces the active hook with the prepared wrapper.
// ReplaceFile never removes the destination first. After a failure the
// destination is classified by content: exact original (old hook still
// active), exact wrapper (late error after a successful install),
// missing (restore the original), or unrecognized (report explicitly,
// never overwrite).
func promoteWrapper(tmpPath, hookPath, backupPath string, wrapper, existing []byte, info os.FileInfo, isSymlink bool, linkTarget string, opts HookInstallOptions) error {
	err := hookRename(tmpPath, hookPath)
	if err == nil {
		return nil
	}
	_ = os.Remove(tmpPath)
	if destMatchesOriginal(hookPath, existing, isSymlink, linkTarget) {
		// Old hook still active; drop the unreferenced backup.
		_ = os.Remove(backupPath)
		return fmt.Errorf("promote hook %s: %w", opts.Name, err)
	}
	if fileMatches(hookPath, wrapper) {
		// The wrapper landed despite the error. Keep its backup.
		return fmt.Errorf("promote hook %s: %w (wrapper installed; backup retained at %s)", opts.Name, err, backupPath)
	}
	if _, statErr := os.Lstat(hookPath); statErr == nil {
		// Unrecognized content: something else wrote the hook.
		// Overwriting could destroy it; leave recovery to the user.
		return fmt.Errorf("promote hook %s failed (%v) and the hook now has unrecognized content; "+
			"no verified hook is active — inspect %s, backup retained at %s",
			opts.Name, err, hookPath, backupPath)
	}
	var restoreErr error
	if isSymlink {
		restoreErr = hookSymlink(linkTarget, hookPath)
	} else {
		restoreErr = hookWriteFile(hookPath, existing, info.Mode().Perm())
	}
	if restoreErr != nil {
		return fmt.Errorf("promote hook %s failed (%v) and the original hook could not be restored (%v); "+
			"no %s hook is active — restore it manually from %s",
			opts.Name, err, restoreErr, opts.Name, backupPath)
	}
	_ = os.Remove(backupPath)
	return fmt.Errorf("promote hook %s: %w (original hook restored)", opts.Name, err)
}

// destMatchesOriginal reports whether the hook at path is still the
// original being wrapped.
func destMatchesOriginal(path string, content []byte, isSymlink bool, linkTarget string) bool {
	if isSymlink {
		got, err := os.Readlink(path)
		return err == nil && got == linkTarget
	}
	return fileMatches(path, content)
}

// fileMatches reports whether path holds exactly want.
func fileMatches(path string, want []byte) bool {
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

func buildSemanticaHookScript(hookName, subcommand string, passArgs bool) []byte {
	args := ""
	if passArgs {
		if hookName == "commit-msg" || subcommand == "commit-msg" {
			args = ` "$1"`
		} else {
			args = ` "$@"`
		}
	}
	redirect := hookOutputRedirect(hookName, subcommand)

	return []byte(fmt.Sprintf(`#!/bin/sh
# %s: %s
# %s
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
[ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0
if command -v semantica >/dev/null 2>&1; then
  semantica hook %s%s%s || true
fi
`, semanticaHookMarker, hookName, semanticaHookMarker, subcommand, args, redirect))
}

func buildSemanticaHookWrapperScript(hookName, userHookFile, subcommand string, passArgs bool) []byte {
	if hookName == "pre-push" || subcommand == "pre-push" {
		return buildPrePushWrapperScript(hookName, userHookFile, subcommand, passArgs)
	}

	args := ""
	if passArgs {
		if hookName == "commit-msg" || subcommand == "commit-msg" {
			args = ` "$1"`
		} else {
			args = ` "$@"`
		}
	}
	redirect := hookOutputRedirect(hookName, subcommand)

	// Preserve Git semantics for user hooks: a non-zero user hook
	// exit blocks the commit. Semantica's own hook stays
	// non-blocking because capture failures should not block Git.
	return []byte(fmt.Sprintf(`#!/bin/sh
# %s (wrapper): %s
# Preserved user hook: %s

HOOK_DIR="$(dirname "$0")"

if [ -x "$HOOK_DIR/%s" ]; then
  "$HOOK_DIR/%s" "$@"
  user_rc=$?
  if [ $user_rc -ne 0 ]; then
    exit $user_rc
  fi
fi

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
[ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0
if command -v semantica >/dev/null 2>&1; then
  semantica hook %s%s%s || true
fi
`, semanticaHookMarker, hookName, userHookFile, userHookFile, userHookFile, subcommand, args, redirect))
}

// buildPrePushWrapperScript replays git's one-shot stdin to both hooks.
// If buffering is unavailable, the preserved user hook keeps raw stdin and
// Semantica is skipped.
func buildPrePushWrapperScript(hookName, userHookFile, subcommand string, passArgs bool) []byte {
	args := ""
	if passArgs {
		args = ` "$@"`
	}
	redirect := hookOutputRedirect(hookName, subcommand)

	return []byte(fmt.Sprintf(`#!/bin/sh
# %s (wrapper): %s
# Preserved user hook: %s

HOOK_DIR="$(dirname "$0")"
USER_HOOK="$HOOK_DIR/%s"

# Allocate a portable temp file for replaying git's pre-push stdin.
STDIN_BUF=""
if STDIN_BUF_CAND="$(mktemp -t semantica-pre-push 2>/dev/null)" && [ -n "$STDIN_BUF_CAND" ]; then
  STDIN_BUF="$STDIN_BUF_CAND"
elif STDIN_BUF_CAND="$(mktemp 2>/dev/null)" && [ -n "$STDIN_BUF_CAND" ]; then
  STDIN_BUF="$STDIN_BUF_CAND"
fi

if [ -n "$STDIN_BUF" ]; then
  # Capture once, then replay to the preserved hook and Semantica.
  trap 'rm -f "$STDIN_BUF"' EXIT
  # If capture fails, block rather than run the user hook on partial input.
  if ! cat > "$STDIN_BUF"; then
    echo "semantica: pre-push: failed to capture git stdin; aborting push to avoid weakening user hook" >&2
    exit 1
  fi

  if [ -x "$USER_HOOK" ]; then
    "$USER_HOOK"%s < "$STDIN_BUF"
    user_rc=$?
    if [ $user_rc -ne 0 ]; then
      exit $user_rc
    fi
  fi

  REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
  [ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0
  if command -v semantica >/dev/null 2>&1; then
    semantica hook %s%s < "$STDIN_BUF"%s || true
  fi
  exit 0
fi

# Fallback: keep the preserved user hook in control when buffering fails.
if [ -x "$USER_HOOK" ]; then
  exec "$USER_HOOK"%s
fi
exit 0
`, semanticaHookMarker, hookName, userHookFile, userHookFile, args, subcommand, args, redirect, args))
}

// preservedHookNamePattern matches the backup filename shape
// generated for preserved user hooks: <hook-name>.user.<digits>.
// The restricted character set excludes path separators,
// whitespace, quotes, and shell metacharacters.
var preservedHookNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*\.user\.[0-9]+$`)

// isValidPreservedHookName reports whether a parsed preserved-hook
// reference can be safely reused in the wrapper script. The name
// must match the generated shape and belong to the hook currently
// being installed.
func isValidPreservedHookName(name, hookName string) bool {
	if !preservedHookNamePattern.MatchString(name) {
		return false
	}
	return strings.HasPrefix(name, hookName+".user.")
}

// parsePreservedUserHook returns the filename of the user hook
// preserved by a Semantica wrapper, or "" if the existing hook
// is the plain (non-wrapper) Semantica form or any other shape.
// The "# Preserved user hook: <filename>" comment line written by
// buildSemanticaHookWrapperScript is both human-readable metadata
// and the reinstall marker that keeps wrappers from being replaced
// by plain Semantica hooks.
//
// The returned value is not yet validated for shape; callers
// that feed it back into the wrapper script must run it through
// isValidPreservedHookName first.
func parsePreservedUserHook(content []byte) string {
	const marker = "# Preserved user hook:"
	for _, line := range bytes.Split(content, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte(marker)) {
			continue
		}
		name := string(bytes.TrimSpace(trimmed[len(marker):]))
		if name == "" {
			return ""
		}
		return name
	}
	return ""
}

func hookOutputRedirect(hookName, subcommand string) string {
	// Only post-commit prints a Semantica summary; other hooks report through doctor.
	if hookName == "post-commit" || subcommand == "post-commit" {
		return ""
	}
	return " >/dev/null 2>&1"
}

// prepareHookFile renders content into a validated executable temp
// file next to path and returns the temp path. The temp file is
// removed on any failure.
func prepareHookFile(path string, content []byte) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := hookCreateTemp(dir, ".semantica-hook-*")
	if err != nil {
		return "", fmt.Errorf("create temp hook %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) (string, error) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%s temp hook %s: %w", step, path, err)
	}
	if _, err := hookTempWrite(tmp, content); err != nil {
		_ = tmp.Close()
		return fail("write", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close", err)
	}
	// os.CreateTemp creates with mode 0o600; hooks need 0o755.
	if err := hookChmod(tmpPath, 0o755); err != nil {
		return fail("chmod", err)
	}
	st, err := os.Stat(tmpPath)
	if err != nil {
		return fail("stat", err)
	}
	if !st.Mode().IsRegular() {
		return fail("validate", fmt.Errorf("not a regular file"))
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return fail("validate", fmt.Errorf("not executable (mode %v)", st.Mode().Perm()))
	}
	return tmpPath, nil
}

// writeExecutableFile installs content at path via a validated temp
// file and platform.ReplaceFile, avoiding in-place truncation and
// destination removal.
func writeExecutableFile(path string, content []byte) error {
	tmpPath, err := prepareHookFile(path, content)
	if err != nil {
		return err
	}
	if err := hookRename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename hook %s: %w", path, err)
	}
	return nil
}

// replaceHookFile is writeExecutableFile for a path with existing
// content. After a failure the destination is classified the same way
// as promoteWrapper: previous content (still active), new content
// (late error), missing (restore), or unrecognized (report, never
// overwrite).
func replaceHookFile(path string, content, prev []byte, prevMode os.FileMode) error {
	tmpPath, err := prepareHookFile(path, content)
	if err != nil {
		return err
	}
	err = hookRename(tmpPath, path)
	if err == nil {
		return nil
	}
	_ = os.Remove(tmpPath)
	if fileMatches(path, prev) {
		return fmt.Errorf("rename hook %s: %w", path, err)
	}
	if fileMatches(path, content) {
		return fmt.Errorf("rename hook %s: %w (new hook installed)", path, err)
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		return fmt.Errorf("rename hook %s failed (%v) and the hook now has unrecognized content; "+
			"no verified hook is active — inspect %s", path, err, path)
	}
	if restoreErr := hookWriteFile(path, prev, prevMode); restoreErr != nil {
		return fmt.Errorf("rename hook %s failed (%v) and the previous hook could not be restored (%v); "+
			"no hook is active — re-run enable", path, err, restoreErr)
	}
	return fmt.Errorf("rename hook %s: %w (previous hook restored)", path, err)
}
