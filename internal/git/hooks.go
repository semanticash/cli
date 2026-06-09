package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	existing, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return writeExecutableFile(hookPath, desired)
		}
		return fmt.Errorf("read existing hook %s: %w", opts.Name, err)
	}

	// If the current hook is Semantica-managed, regenerate it. A
	// wrapper must stay a wrapper so any preserved user hook remains
	// in the execution chain across re-enable or upgrade.
	if bytes.Contains(existing, []byte(semanticaHookMarker)) {
		userHookFile := parsePreservedUserHook(existing)
		if userHookFile == "" {
			// Plain Semantica hook (no preserved wrapper). Safe to
			// regenerate as the plain form.
			return writeExecutableFile(hookPath, desired)
		}
		// The parsed filename is written back into the wrapper
		// script. Only accept the generated backup filename shape;
		// damaged or hand-edited wrappers are left untouched.
		if !isValidPreservedHookName(userHookFile, opts.Name) {
			return fmt.Errorf("hook %s appears to be a damaged or hand-edited Semantica wrapper "+
				"(preserved-hook reference %q does not match the generated shape <hook>.user.<unix-ms>); "+
				"inspect %s manually before retrying", opts.Name, userHookFile, hookPath)
		}
		wrapper := buildSemanticaHookWrapperScript(opts.Name, userHookFile, opts.Subcommand, opts.PassArgs)
		return writeExecutableFile(hookPath, wrapper)
	}

	// Otherwise, preserve existing hook and install wrapper.
	backupName := fmt.Sprintf("%s.user.%d", opts.Name, time.Now().UnixMilli())
	backupPath := filepath.Join(hooksDir, backupName)

	if err := os.Rename(hookPath, backupPath); err != nil {
		return fmt.Errorf("backup existing hook %s: %w", opts.Name, err)
	}

	wrapper := buildSemanticaHookWrapperScript(opts.Name, backupName, opts.Subcommand, opts.PassArgs)
	return writeExecutableFile(hookPath, wrapper)
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
// generated for preserved user hooks: <hook-name>.user.<unix-ms>.
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

// writeExecutableFile writes through a temp file in the hooks
// directory, then replaces the target with platform.SafeRename.
// This avoids in-place truncation and partial writes if the
// process is interrupted. On Windows, replacement is best-effort
// because the platform cannot atomically rename over an existing
// file.
func writeExecutableFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".semantica-hook-*")
	if err != nil {
		return fmt.Errorf("create temp hook %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp hook %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp hook %s: %w", path, err)
	}
	// os.CreateTemp creates with mode 0o600; hooks need 0o755.
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod temp hook %s: %w", path, err)
	}
	if err := platform.SafeRename(tmpPath, path); err != nil {
		return fmt.Errorf("rename hook %s: %w", path, err)
	}
	renamed = true
	return nil
}
