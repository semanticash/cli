package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const semanticaHookMarker = "Semantica git hook"

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

	// If it's already ours, overwrite (update).
	if bytes.Contains(existing, []byte(semanticaHookMarker)) {
		return writeExecutableFile(hookPath, desired)
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
# %s (wrapper): %s
# Preserved user hook: %s

HOOK_DIR="$(dirname "$0")"

if [ -x "$HOOK_DIR/%s" ]; then
  "$HOOK_DIR/%s" "$@" || true
fi

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
[ -f "$REPO_ROOT/.semantica/enabled" ] || exit 0
if command -v semantica >/dev/null 2>&1; then
  semantica hook %s%s%s || true
fi
`, semanticaHookMarker, hookName, userHookFile, userHookFile, userHookFile, subcommand, args, redirect))
}

func hookOutputRedirect(hookName, subcommand string) string {
	if hookName == "post-commit" || subcommand == "post-commit" {
		return ""
	}
	return " >/dev/null 2>&1"
}

func writeExecutableFile(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o755); err != nil {
		return fmt.Errorf("write hook %s: %w", path, err)
	}
	return nil
}
