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
)

type Repo struct {
	root string
}

func OpenRepo(repoPath string) (*Repo, error) {
	start := repoPath
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}

	root, err := findGitRoot(start)
	if err != nil {
		return nil, err
	}
	return &Repo{root: root}, nil
}

func (r *Repo) Root() string { return r.root }

func (r *Repo) ListFilesFromGit(ctx context.Context) ([]string, error) {
	// Includes:
	//  - tracked files (--cached)
	//  - untracked files (--others)
	// Excludes:
	//  - ignored files (--exclude-standard)
	//
	// -z ensures NUL-separated output, robust for weird filenames.
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = r.root

	out, err := cmd.Output()
	if err != nil {
		// If git wrote to stderr, wrap it for better debugging.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git ls-files failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git ls-files failed: %w", err)
	}

	// Split by NUL. Last element is often empty if output ends with NUL.
	parts := bytes.Split(out, []byte{0})

	files := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}

		rel := string(p)

		// Normalize to OS-specific separators
		// Git outputs paths with '/' separators
		rel = filepath.FromSlash(rel)

		// Safety guard: do not allow Semantica's own folder even if somehow listed.
		if rel == ".semantica" || strings.HasPrefix(rel, ".semantica"+string(filepath.Separator)) {
			continue
		}

		files = append(files, rel)
	}

	return files, nil
}

func (r *Repo) ReadFile(relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.root, relPath))
}

func (r *Repo) WriteFile(relPath string, b []byte) error {
	abs := filepath.Join(r.root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", relPath, err)
	}
	if err := os.WriteFile(abs, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	return nil
}

// RestoreFile restores a file from a checkpoint manifest. It handles both
// regular files (with correct mode bits) and symlinks. For backward
// compatibility, if mode is 0 it defaults to 0644.
func (r *Repo) RestoreFile(relPath string, content []byte, mode os.FileMode, isSymlink bool, linkTarget string) error {
	abs := filepath.Join(r.root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", relPath, err)
	}

	// Remove any existing entry (file or symlink) before restoring.
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", relPath, err)
	}

	if isSymlink {
		if err := os.Symlink(linkTarget, abs); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", relPath, linkTarget, err)
		}
		return nil
	}

	if mode == 0 {
		mode = 0o644
	}
	if err := os.WriteFile(abs, content, mode); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	return nil
}

func (r *Repo) RemoveFile(relPath string) error {
	abs := filepath.Join(r.root, relPath)
	// If missing, ignore
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", relPath, err)
	}
	return nil
}

// IsDirty returns true if the working tree has any changes:
// -modified, staged, deleted, untracked
func (r *Repo) IsDirty(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z")
	cmd.Dir = r.root

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return false, fmt.Errorf("git status failed: %w: %s", err, string(ee.Stderr))
		}
		return false, fmt.Errorf("git status failed: %w", err)
	}

	return len(bytes.TrimSpace(out)) > 0, nil
}

// ResolveRef resolves a git ref (HEAD, branch name, tag, commit prefix) to a
// full commit hash. Returns an error if the ref is not valid.
func (r *Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a valid git ref: %s", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentBranch returns the current branch name (e.g. "main", "feature-x").
// Returns "HEAD" for detached HEAD state.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get current branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoteURL returns the URL of the "origin" remote.
func (r *Repo) RemoteURL(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get remote.origin.url: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) CommitSubject(ctx context.Context, commitHash string) (string, error) {
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("commit hash is empty")
	}
	cmd := exec.CommandContext(ctx, "git", "show", "-s", "--format=%s", commitHash)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parentForCommit resolves the first parent of a commit hash, returning the
// magic empty-tree SHA for the initial commit (no parents).
func (r *Repo) parentForCommit(ctx context.Context, hash string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--parents", "-n1", hash)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-list failed for %s: %w", hash, err)
	}

	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 2 {
		// Initial commit - diff against empty tree
		return "4b825dc642cb6eb9a060e54bf8d69288fbee4904", nil
	}
	// Use first parent (handles both normal and merge commits)
	return parts[1], nil
}

// DiffCached returns the unified diff of staged changes against HEAD.
// Used by the commit-msg hook to compute AI attribution before the commit
// is finalized (the commit hash doesn't exist yet at that point).
func (r *Repo) DiffCached(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--no-color")
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff --cached failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff --cached failed: %w", err)
	}
	return out, nil
}

// DiffAll returns a unified diff of all uncommitted changes: staged,
// unstaged, and untracked files. Untracked files are represented as
// new-file diffs so the LLM sees their full content.
func (r *Repo) DiffAll(ctx context.Context) ([]byte, error) {
	// Staged + unstaged tracked changes.
	cmd := exec.CommandContext(ctx, "git", "diff", "HEAD", "--no-color")
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		// HEAD may not exist (initial commit) - fall back to staged + unstaged.
		cmd2 := exec.CommandContext(ctx, "git", "diff", "--no-color")
		cmd2.Dir = r.root
		out, err = cmd2.Output()
		if err != nil {
			return nil, fmt.Errorf("git diff failed: %w", err)
		}
	}

	// Append untracked files as synthetic new-file diffs.
	untracked, _ := r.listUntrackedFiles(ctx)
	for _, path := range untracked {
		content, readErr := os.ReadFile(filepath.Join(r.root, path))
		if readErr != nil {
			continue
		}
		// Skip binary files (files with null bytes in the first 512 bytes).
		preview := content
		if len(preview) > 512 {
			preview = preview[:512]
		}
		isBinary := false
		for _, b := range preview {
			if b == 0 {
				isBinary = true
				break
			}
		}
		if isBinary {
			continue
		}

		lines := strings.Split(string(content), "\n")
		var buf strings.Builder
		fmt.Fprintf(&buf, "diff --git a/%s b/%s\n", path, path)
		fmt.Fprintf(&buf, "new file mode 100644\n")
		fmt.Fprintf(&buf, "--- /dev/null\n")
		fmt.Fprintf(&buf, "+++ b/%s\n", path)
		fmt.Fprintf(&buf, "@@ -0,0 +1,%d @@\n", len(lines))
		for _, line := range lines {
			fmt.Fprintf(&buf, "+%s\n", line)
		}
		out = append(out, buf.String()...)
	}

	return out, nil
}

// listUntrackedFiles returns repo-relative paths of untracked files.
func (r *Repo) listUntrackedFiles(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func (r *Repo) DiffForCommit(ctx context.Context, hash string) ([]byte, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, fmt.Errorf("commit hash is empty")
	}

	parent, err := r.parentForCommit(ctx, hash)
	if err != nil {
		return nil, err
	}

	diffCmd := exec.CommandContext(ctx, "git", "diff", "--no-color", parent, hash)
	diffCmd.Dir = r.root
	diffOut, err := diffCmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	return diffOut, nil
}

// FileStat holds per-file line counts from a diff.
type FileStat struct {
	Path    string
	Added   int
	Deleted int
}

// DiffStatForCommit returns per-file added/deleted line counts for the given
// commit using git diff --numstat. Binary files are included with 0/0 counts.
func (r *Repo) DiffStatForCommit(ctx context.Context, hash string) ([]FileStat, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, fmt.Errorf("commit hash is empty")
	}

	parent, err := r.parentForCommit(ctx, hash)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", "-M", parent, hash)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff --numstat failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff --numstat failed: %w", err)
	}

	var stats []FileStat
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: <added>\t<deleted>\t<path>
		// Binary files show: -\t-\t<path>
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		var added, deleted int
		if parts[0] != "-" {
			if _, err := fmt.Sscanf(parts[0], "%d", &added); err != nil {
				continue // skip malformed line
			}
		}
		if parts[1] != "-" {
			if _, err := fmt.Sscanf(parts[1], "%d", &deleted); err != nil {
				continue
			}
		}
		path := filepath.ToSlash(strings.TrimPrefix(parts[2], "./"))
		stats = append(stats, FileStat{Path: path, Added: added, Deleted: deleted})
	}
	return stats, nil
}

// ChangedFilesForCommit returns the repo-relative paths (forward slashes) of
// files changed by the given commit. Uses git diff-tree, the canonical
// plumbing command for listing files touched by a commit.
func (r *Repo) ChangedFilesForCommit(ctx context.Context, hash string) ([]string, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, fmt.Errorf("commit hash is empty")
	}

	cmd := exec.CommandContext(ctx, "git", "diff-tree", "--no-commit-id", "--name-only", "-r", "-M", "--root", hash)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff-tree failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff-tree failed: %w", err)
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = filepath.ToSlash(strings.TrimPrefix(line, "./"))
		files = append(files, line)
	}
	return files, nil
}

func findGitRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}

	for {
		gitPath := filepath.Join(dir, ".git")

		if st, err := os.Stat(gitPath); err == nil {
			// Accept both directory (.git) and file (.git) for worktrees.
			if st.IsDir() || st.Mode().IsRegular() {
				canonical, err := filepath.EvalSymlinks(dir)
				if err == nil {
					return canonical, nil
				}
				return dir, nil
			}
		}

		parent := filepath.Dir(dir)
		// If we reached filesystem root, stop
		if parent == dir {
			break
		}

		dir = parent
	}

	return "", fmt.Errorf("not a git repository (or any of the parent directories)")
}

// DefaultBaseRef returns the best-guess default branch ref for the repository.
// Resolution order: refs/remotes/origin/HEAD, origin/main, origin/master, main, master.
func (r *Repo) DefaultBaseRef(ctx context.Context) (string, error) {
	candidates := []string{
		"refs/remotes/origin/HEAD",
		"origin/main",
		"origin/master",
		"main",
		"master",
	}
	for _, ref := range candidates {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref)
		cmd.Dir = r.root
		if err := cmd.Run(); err == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("cannot determine default branch; use --base to specify")
}

// MergeBase returns the best common ancestor of two refs.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-base", a, b)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("merge-base %s %s: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DiffBetween returns the unified diff between two refs (three-dot: from
// merge-base of a and b to b). This shows only changes introduced on the
// feature branch, not upstream drift.
func (r *Repo) DiffBetween(ctx context.Context, base, head string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-color", base+"..."+head)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("diff %s...%s: %w", base, head, err)
	}
	return out, nil
}

// CommitSubjectsBetween returns commit subject lines (no hashes) for
// commits reachable from head but not from base, newest first, capped at limit.
func (r *Repo) CommitSubjectsBetween(ctx context.Context, base, head string, limit int) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%s",
		fmt.Sprintf("-%d", limit), base+".."+head)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("log %s..%s: %w", base, head, err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}
