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

	"github.com/semanticash/cli/internal/platform"
)

type Repo struct {
	root string
}

// cleanGitOutput strips Windows \r\n line endings from git command output.
// Git on Windows may produce \r\n when core.autocrlf is set.
func cleanGitOutput(out []byte) string {
	return strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n")
}

// FindRoot resolves the given path to the enclosing git repository root,
// returning the canonical (symlink-resolved) path. Worktrees backed by a
// .git file are accepted. Returns an error if no .git is found up to the
// filesystem root.
//
// Callers that need a full *Repo handle should use OpenRepo. FindRoot is
// the lightweight option for code that only cares about whether a path
// belongs to a repository and which one.
func FindRoot(start string) (string, error) {
	return findGitRoot(start)
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

// gitCmd creates a git command configured for this repo. On Windows,
// suppresses console window allocation for detached worker processes.
func (r *Repo) gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.root
	platform.HideWindow(cmd)
	return cmd
}

func (r *Repo) ListFilesFromGit(ctx context.Context) ([]string, error) {
	// Includes:
	//  - tracked files (--cached)
	//  - untracked files (--others)
	// Excludes:
	//  - ignored files (--exclude-standard)
	//
	// -z ensures NUL-separated output, robust for weird filenames.
	cmd := r.gitCmd(ctx, "ls-files", "--cached", "--others", "--exclude-standard", "-z")

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
	cmd := r.gitCmd(ctx, "status", "--porcelain", "-z")

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return false, fmt.Errorf("git status failed: %w: %s", err, string(ee.Stderr))
		}
		return false, fmt.Errorf("git status failed: %w", err)
	}

	return len(bytes.TrimSpace(out)) > 0, nil
}

// StatusShort returns `git status --short` output (one line per
// file, e.g. " M path/to/file"). Used by the handoff bundle to
// summarize uncommitted changes a fresh agent session needs to
// know about.
func (r *Repo) StatusShort(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "status", "--short", "--no-renames")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git status --short failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git status --short failed: %w", err)
	}
	return cleanGitOutput(out), nil
}

// Commit is one entry in the result of LogSince. Used by the
// handoff bundle to render each commit's subject and (when
// non-empty) body separately, so a session-summary commit with
// substantive details doesn't get truncated to its first line.
type Commit struct {
	// ShortHash is git's abbreviated hash (typically 7 chars).
	ShortHash string

	// Subject is the first line of the commit message.
	Subject string

	// Body is the trimmed remainder of the commit message; empty
	// when the commit has no body. Already newline-normalized.
	Body string
}

// recordSep and fieldSep separate git-log records and per-record
// fields. The byte values are deliberately non-printable and
// outside what commit messages legitimately contain, so a commit
// whose subject or body includes a literal "\n" or "|" cannot
// confuse the parser. ASCII Record-Separator (0x1e) and
// Unit-Separator (0x1f) are the standard control-byte choice for
// this pattern.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// LogSince returns commits whose author-date is at or after the
// given time, capped at limit entries (default 20 when limit<=0).
// Each Commit carries the short hash, subject, and body so the
// handoff bundle can render them with their full context rather
// than collapsing each commit to a single line.
func (r *Repo) LogSince(ctx context.Context, since time.Time, limit int) ([]Commit, error) {
	if limit <= 0 {
		limit = 20
	}
	// Format: <short-hash> US <subject> US <body> RS.
	// Delimit records and fields with control bytes so
	// commit messages containing newlines, pipes, or anything else
	// printable cannot break parsing.
	format := fmt.Sprintf("%%h%s%%s%s%%b%s", fieldSep, fieldSep, recordSep)
	args := []string{
		"log",
		fmt.Sprintf("--since=%d", since.Unix()),
		"--max-count", fmt.Sprintf("%d", limit),
		"--pretty=format:" + format,
		"--no-color",
	}
	cmd := r.gitCmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git log failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git log failed: %w", err)
	}
	raw := strings.TrimRight(string(out), "\n")
	if raw == "" {
		return nil, nil
	}
	// Split on the record separator. Each record is then
	// further split on the field separator (3 fields).
	records := strings.Split(raw, recordSep)
	commits := make([]Commit, 0, len(records))
	for _, rec := range records {
		rec = strings.TrimLeft(rec, "\n") // git adds a leading \n between records
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, fieldSep, 3)
		if len(parts) < 3 {
			// Defensive: a malformed record skips silently
			// rather than failing the whole bundle assembly.
			continue
		}
		commits = append(commits, Commit{
			ShortHash: parts[0],
			Subject:   parts[1],
			Body:      strings.TrimSpace(parts[2]),
		})
	}
	return commits, nil
}

// DiffWorkingTree returns the combined unstaged + staged diff
// against HEAD. Used by the handoff bundle to surface uncommitted
// changes; callers redact the result before including it in any
// agent-visible payload.
func (r *Repo) DiffWorkingTree(ctx context.Context) ([]byte, error) {
	cmd := r.gitCmd(ctx, "diff", "--no-color", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff HEAD failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff HEAD failed: %w", err)
	}
	return out, nil
}

// ResolveRef resolves a git ref (HEAD, branch name, tag, commit prefix) to a
// full commit hash. Returns an error if the ref is not valid.
func (r *Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	cmd := r.gitCmd(ctx, "rev-parse", "--verify", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a valid git ref: %s", ref)
	}
	return cleanGitOutput(out), nil
}

// CurrentBranch returns the current branch name (e.g. "main", "feature-x").
// Returns "HEAD" for detached HEAD state.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get current branch: %w", err)
	}
	return cleanGitOutput(out), nil
}

// RemoteURL returns the URL of the "origin" remote.
func (r *Repo) RemoteURL(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get remote.origin.url: %w", err)
	}
	return cleanGitOutput(out), nil
}

func (r *Repo) CommitSubject(ctx context.Context, commitHash string) (string, error) {
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("commit hash is empty")
	}
	cmd := r.gitCmd(ctx, "show", "-s", "--format=%s", commitHash)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return cleanGitOutput(out), nil
}

// CommitFormat runs `git show -s --format=<format> <commitHash>` and
// returns the trimmed output. Callers compose multi-field formats
// (e.g. "%H%n%an%n%ai%n%s") and parse the result by line. This is
// the single git-show entry point for callers that need more than
// just the subject.
func (r *Repo) CommitFormat(ctx context.Context, commitHash, format string) (string, error) {
	if strings.TrimSpace(commitHash) == "" {
		return "", fmt.Errorf("commit hash is empty")
	}
	if format == "" {
		return "", fmt.Errorf("format is empty")
	}
	cmd := r.gitCmd(ctx, "show", "-s", "--format="+format, commitHash)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return cleanGitOutput(out), nil
}

// parentForCommit resolves the first parent of a commit hash, returning the
// magic empty-tree SHA for the initial commit (no parents).
func (r *Repo) parentForCommit(ctx context.Context, hash string) (string, error) {
	cmd := r.gitCmd(ctx, "rev-list", "--parents", "-n1", hash)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-list failed for %s: %w", hash, err)
	}

	parts := strings.Fields(cleanGitOutput(out))
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
	cmd := r.gitCmd(ctx, "diff", "--cached", "--no-color")
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
	cmd := r.gitCmd(ctx, "diff", "HEAD", "--no-color")
	out, err := cmd.Output()
	if err != nil {
		// HEAD may not exist (initial commit) - fall back to staged + unstaged.
		cmd2 := r.gitCmd(ctx, "diff", "--no-color")
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
	cmd := r.gitCmd(ctx, "ls-files", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(cleanGitOutput(out), "\n") {
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

	diffCmd := r.gitCmd(ctx, "diff", "--no-color", parent, hash)
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

	cmd := r.gitCmd(ctx, "diff", "--numstat", "-M", parent, hash)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff --numstat failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff --numstat failed: %w", err)
	}

	var stats []FileStat
	for _, line := range strings.Split(cleanGitOutput(out), "\n") {
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

	cmd := r.gitCmd(ctx, "diff-tree", "--no-commit-id", "--name-only", "-r", "-M", "--root", hash)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git diff-tree failed: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git diff-tree failed: %w", err)
	}

	var files []string
	for _, line := range strings.Split(cleanGitOutput(out), "\n") {
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
		cmd := r.gitCmd(ctx, "rev-parse", "--verify", "--quiet", ref)
		if err := cmd.Run(); err == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("cannot determine default branch; use --base to specify")
}

// MergeBase returns the best common ancestor of two refs.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	cmd := r.gitCmd(ctx, "merge-base", a, b)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("merge-base %s %s: %w", a, b, err)
	}
	return cleanGitOutput(out), nil
}

// DiffBetween returns the unified diff between two refs (three-dot: from
// merge-base of a and b to b). This shows only changes introduced on the
// feature branch, not upstream drift.
func (r *Repo) DiffBetween(ctx context.Context, base, head string) ([]byte, error) {
	cmd := r.gitCmd(ctx, "diff", "--no-color", base+"..."+head)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("diff %s...%s: %w", base, head, err)
	}
	return out, nil
}

// CommitSubjectsBetween returns commit subject lines (no hashes) for
// commits reachable from head but not from base, newest first, capped at limit.
func (r *Repo) CommitSubjectsBetween(ctx context.Context, base, head string, limit int) ([]string, error) {
	cmd := r.gitCmd(ctx, "log", "--format=%s",
		fmt.Sprintf("-%d", limit), base+".."+head)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("log %s..%s: %w", base, head, err)
	}
	raw := cleanGitOutput(out)
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// CountCommitsBetween returns the exact number of commits reachable
// from head but not from base. Used by callers that need an honest
// "dropped N commits" report when truncating: a limit-clamped log
// query cannot reveal the real total.
func (r *Repo) CountCommitsBetween(ctx context.Context, base, head string) (int, error) {
	cmd := r.gitCmd(ctx, "rev-list", "--count", base+".."+head)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("rev-list --count %s..%s: %w", base, head, err)
	}
	raw := cleanGitOutput(out)
	if raw == "" {
		return 0, nil
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("parse rev-list --count output %q: non-numeric character", raw)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
