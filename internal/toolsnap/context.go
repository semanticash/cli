// Package toolsnap captures ephemeral workspace snapshots around
// agent-executed tool windows. Snapshots are Git trees stored in an
// isolated bare object store under .semantica, with the user
// repository's object database as a read-only alternate. The user
// repository's objects and refs are never modified.
package toolsnap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// RepoContext identifies the worktree and object database a snapshot
// operates against. All paths are absolute and symlink-canonicalized.
type RepoContext struct {
	// WorktreeRoot is the top-level directory of the active worktree.
	WorktreeRoot string
	// GitDir is the worktree's git directory (per-worktree for linked
	// worktrees).
	GitDir string
	// CommonDir is the shared git directory holding the object database.
	CommonDir string
	// ObjectFormat is "sha1" or "sha256".
	ObjectFormat string
	// WorktreeID is a stable identifier for this worktree, used to
	// namespace snapshot refs.
	WorktreeID string
}

// ResolveRepoContext discovers the repository context for the given
// directory. It fails on bare repositories: snapshots capture worktree
// state, so a worktree is required.
func ResolveRepoContext(ctx context.Context, dir string) (RepoContext, error) {
	out, err := gitOutput(ctx, dir,
		"rev-parse",
		"--is-bare-repository",
		"--show-toplevel",
		"--absolute-git-dir",
		"--git-common-dir",
		"--show-object-format",
	)
	if err != nil {
		return RepoContext{}, fmt.Errorf("toolsnap: resolve repository: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		return RepoContext{}, fmt.Errorf("toolsnap: unexpected rev-parse output: %q", out)
	}
	if lines[0] == "true" {
		return RepoContext{}, fmt.Errorf("toolsnap: bare repository has no worktree to snapshot")
	}

	rc := RepoContext{}
	if rc.WorktreeRoot, err = canonicalPath(lines[1]); err != nil {
		return RepoContext{}, fmt.Errorf("toolsnap: canonicalize worktree root: %w", err)
	}
	if rc.GitDir, err = canonicalPath(lines[2]); err != nil {
		return RepoContext{}, fmt.Errorf("toolsnap: canonicalize git dir: %w", err)
	}
	// --git-common-dir may be relative to the worktree.
	commonDir := lines[3]
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(rc.WorktreeRoot, commonDir)
	}
	if rc.CommonDir, err = canonicalPath(commonDir); err != nil {
		return RepoContext{}, fmt.Errorf("toolsnap: canonicalize common dir: %w", err)
	}

	rc.ObjectFormat = strings.TrimSpace(lines[4])
	if rc.ObjectFormat != "sha1" && rc.ObjectFormat != "sha256" {
		return RepoContext{}, fmt.Errorf("toolsnap: unsupported object format %q", rc.ObjectFormat)
	}

	rc.WorktreeID = worktreeID(rc.GitDir, rc.CommonDir)
	return rc, nil
}

// worktreeID returns a stable identity for a worktree. Linked worktrees
// use Git's repository-local administrative directory name.
func worktreeID(gitDir, commonDir string) string {
	if gitDir == commonDir {
		return "main"
	}
	return sanitizeRefComponent(filepath.Base(gitDir))
}

// sanitizeRefComponent returns a collision-resistant Git ref component.
// The prefix is readable; the 128-bit digest preserves identity.
func sanitizeRefComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	prefix := b.String()
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	if prefix == "" {
		prefix = "id"
	}
	sum := sha256.Sum256([]byte(s))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}

func canonicalPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// scrubbedGitVars contains inherited variables that can redirect Git's
// repository, object database, index, or discovery behavior.
var scrubbedGitVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_INDEX_FILE",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// gitEnv removes inherited settings that can redirect repository access.
// GIT_NO_LAZY_FETCH supplements store isolation on Git 2.45 and later.
// GIT_OPTIONAL_LOCKS keeps read-only commands from refreshing user state.
func gitEnv(extra []string) []string {
	env := make([]string, 0, len(scrubbedGitVars)+len(extra)+4)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && isScrubbedGitVar(key) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
	return append(env, extra...)
}

func isScrubbedGitVar(key string) bool {
	for _, v := range scrubbedGitVars {
		// Environment names are case-insensitive on Windows.
		if strings.EqualFold(key, v) {
			return true
		}
	}
	return false
}

// storeGitEnv also disables inherited Git configuration. Store commands
// must not discover remotes or promisor settings that could trigger a fetch.
func storeGitEnv(extra []string) []string {
	base := gitEnv(nil)
	env := make([]string, 0, len(base)+len(extra)+2)
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if ok && len(key) >= len("GIT_CONFIG") && strings.EqualFold(key[:len("GIT_CONFIG")], "GIT_CONFIG") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	return append(env, extra...)
}

// gitOutput runs git in dir under the scrubbed capture environment.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutputEnv(ctx, dir, gitEnv(nil), args...)
}

// gitOutputEnv runs git in dir under an explicit environment.
func gitOutputEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	platform.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
