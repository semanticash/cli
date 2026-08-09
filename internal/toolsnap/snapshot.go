package toolsnap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// Snapshot is one captured workspace state: a Git tree in the isolated
// store plus the repository positions it was built against.
type Snapshot struct {
	TreeHash  string
	HeadHash  string
	DirtyPath []string
}

// CaptureBefore builds a tree representing the current worktree
// content: the HEAD tree with every dirty, staged, type-changed, or
// untracked path replaced by its literal worktree bytes and deletions
// removed. Unchanged subtrees keep their existing hashes, so cost
// scales with dirty paths, not repository size.
func (s *Store) CaptureBefore(ctx context.Context) (Snapshot, error) {
	headCommit, headTree, err := s.resolveHead(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	paths, err := dirtyPaths(ctx, s.repo, s.maxPaths())
	if err != nil {
		return Snapshot{}, err
	}
	// Clean fast path: with no dirty paths the worktree tree is the
	// HEAD tree, already stored in the repository. No index, no tree
	// writing, no store objects.
	if len(paths) == 0 {
		return Snapshot{TreeHash: headTree, HeadHash: headCommit}, nil
	}
	tree, err := s.buildTree(ctx, headTree, paths)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{TreeHash: tree, HeadHash: headCommit, DirtyPath: paths}, nil
}

// resolveHead returns the HEAD commit and tree hashes. An unborn
// branch resolves to the empty tree.
func (s *Store) resolveHead(ctx context.Context) (commit, tree string, err error) {
	out, err := gitOutput(ctx, s.repo.WorktreeRoot, "rev-parse", "HEAD", "HEAD^{tree}")
	if err != nil {
		if strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "ambiguous argument") {
			empty, eerr := s.emptyTree(ctx)
			return "", empty, eerr
		}
		return "", "", fmt.Errorf("toolsnap: resolve HEAD: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("toolsnap: unexpected HEAD resolution: %q", out)
	}
	return lines[0], lines[1], nil
}

func (s *Store) emptyTree(ctx context.Context) (string, error) {
	out, err := s.gitStdin(ctx, nil, nil, "mktree")
	if err != nil {
		return "", fmt.Errorf("toolsnap: empty tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// buildTree applies worktree changes to the HEAD tree through a
// temporary index. Unchanged subtrees retain their object IDs.
func (s *Store) buildTree(ctx context.Context, headTree string, paths []string) (string, error) {
	idx, err := os.CreateTemp(s.Dir, "snap-index-*")
	if err != nil {
		return "", fmt.Errorf("toolsnap: temp index: %w", err)
	}
	idxPath := idx.Name()
	_ = idx.Close()
	_ = os.Remove(idxPath) // read-tree recreates it; avoid an empty-file collision
	defer func() { _ = os.Remove(idxPath) }()

	env := []string{"GIT_INDEX_FILE=" + idxPath}
	if _, err := s.gitStdin(ctx, env, nil, "read-tree", headTree); err != nil {
		return "", fmt.Errorf("toolsnap: read-tree: %w", err)
	}

	entries, written, err := s.hashWorktreePaths(ctx, paths)
	if err != nil {
		return "", err
	}
	// Enforce the budget against the blobs Git actually hashed. Objects
	// written by a rejected capture remain unreachable and are pruned by
	// snapshot-store maintenance.
	if len(written) > 0 {
		bytesRead, err := s.blobSizeSum(ctx, written)
		if err != nil {
			return "", err
		}
		if bytesRead > s.maxBytes() {
			return "", &PartialError{
				Reason: ReasonByteLimit,
				Detail: fmt.Sprintf("hashed content exceeds the %d byte limit", s.maxBytes()),
			}
		}
	}
	if len(entries) > 0 {
		var in bytes.Buffer
		for _, e := range entries {
			in.WriteString(e)
			in.WriteByte(0)
		}
		if _, err := s.gitStdin(ctx, env, in.Bytes(), "update-index", "-z", "--index-info"); err != nil {
			return "", fmt.Errorf("toolsnap: update-index: %w", err)
		}
	}

	out, err := s.gitStdin(ctx, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("toolsnap: write-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// indexEntry is one update-index record. Removals use an all-zero
// object ID; every other entry references a blob hashed by capture.
type indexEntry struct {
	mode string
	hash string
	path string
}

func (e indexEntry) isRemoval() bool { return e.mode == "000000" }

func (e indexEntry) indexInfo() string {
	return fmt.Sprintf("%s %s 0\t%s", e.mode, e.hash, e.path)
}

// hashWorktreePaths converts candidate paths into index entries. The
// worktree is authoritative: present files use literal bytes and missing
// files become removals. Hashing bypasses user-configured clean filters.
func (s *Store) hashWorktreePaths(ctx context.Context, paths []string) ([]string, []string, error) {
	zero := strings.Repeat("0", hashLen(s.repo.ObjectFormat))
	typed := make([]indexEntry, 0, len(paths))
	type regularFile struct {
		path string
		mode string
	}
	type symlinkFile struct {
		path   string
		target string
	}
	var regular []regularFile
	var symlinks []symlinkFile
	var bytesToRead int64

	// Classify and budget all candidates before writing blobs.
	for _, p := range paths {
		abs := filepath.Join(s.repo.WorktreeRoot, filepath.FromSlash(p))
		fi, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				typed = append(typed, indexEntry{mode: "000000", hash: zero, path: p})
				continue
			}
			return nil, nil, fmt.Errorf("toolsnap: lstat %s: %w", p, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(abs)
			if err != nil {
				return nil, nil, fmt.Errorf("toolsnap: readlink %s: %w", p, err)
			}
			bytesToRead += int64(len(target))
			symlinks = append(symlinks, symlinkFile{path: p, target: target})
		case fi.Mode().IsRegular():
			bytesToRead += fi.Size()
			mode := "100644"
			if fi.Mode()&0o111 != 0 {
				mode = "100755"
			}
			regular = append(regular, regularFile{path: p, mode: mode})
		default:
			// Git trees cannot represent sockets, pipes, or devices.
			return nil, nil, &PartialError{
				Reason: ReasonUnsupportedPath,
				Detail: fmt.Sprintf("%s is not a regular file or symlink", p),
			}
		}
		if bytesToRead > s.maxBytes() {
			return nil, nil, &PartialError{
				Reason: ReasonByteLimit,
				Detail: fmt.Sprintf("candidate content exceeds %d byte limit at %s", s.maxBytes(), p),
			}
		}
	}

	// Write candidate blobs after preflight succeeds.
	for _, sl := range symlinks {
		hash, err := s.gitStdin(ctx, nil, []byte(sl.target), "hash-object", "-w", "-t", "blob", "--stdin")
		if err != nil {
			return nil, nil, fmt.Errorf("toolsnap: hash symlink %s: %w", sl.path, err)
		}
		typed = append(typed, indexEntry{mode: "120000", hash: strings.TrimSpace(hash), path: sl.path})
	}

	// hash-object --stdin-paths is newline-delimited with no -z form,
	// so paths containing a newline go through per-file argv calls.
	var batch []regularFile
	for _, rf := range regular {
		if strings.ContainsRune(rf.path, '\n') {
			abs := filepath.Join(s.repo.WorktreeRoot, filepath.FromSlash(rf.path))
			hash, err := s.gitStdin(ctx, nil, nil,
				"hash-object", "-w", "-t", "blob", "--no-filters", "--", abs)
			if err != nil {
				return nil, nil, fmt.Errorf("toolsnap: hash blob %q: %w", rf.path, err)
			}
			typed = append(typed, indexEntry{mode: rf.mode, hash: strings.TrimSpace(hash), path: rf.path})
			continue
		}
		batch = append(batch, rf)
	}
	if len(batch) > 0 {
		var in bytes.Buffer
		for _, rf := range batch {
			in.WriteString(rf.path)
			in.WriteByte('\n')
		}
		out, err := s.gitStdinDir(ctx, s.repo.WorktreeRoot, nil, in.Bytes(),
			"hash-object", "-w", "-t", "blob", "--no-filters", "--stdin-paths")
		if err != nil {
			return nil, nil, fmt.Errorf("toolsnap: hash worktree blobs: %w", err)
		}
		hashes := strings.Fields(out)
		if len(hashes) != len(batch) {
			return nil, nil, fmt.Errorf("toolsnap: hashed %d of %d blobs", len(hashes), len(batch))
		}
		for i, rf := range batch {
			typed = append(typed, indexEntry{mode: rf.mode, hash: hashes[i], path: rf.path})
		}
	}

	// Derive index records and byte-accounted blob hashes together.
	entries := make([]string, 0, len(typed))
	var written []string
	for _, e := range typed {
		entries = append(entries, e.indexInfo())
		if !e.isRemoval() {
			written = append(written, e.hash)
		}
	}
	return entries, written, nil
}

// blobSizeSum returns the total uncompressed size of the given blobs.
func (s *Store) blobSizeSum(ctx context.Context, hashes []string) (int64, error) {
	in := strings.Join(hashes, "\n") + "\n"
	out, err := s.gitStdin(ctx, nil, []byte(in), "cat-file", "--batch-check")
	if err != nil {
		return 0, fmt.Errorf("toolsnap: batch-check written blobs: %w", err)
	}
	var total int64
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(hashes) {
		return 0, fmt.Errorf("toolsnap: batch-check returned %d of %d records", len(lines), len(hashes))
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "blob" {
			return 0, fmt.Errorf("toolsnap: unexpected batch-check record %q", line)
		}
		var size int64
		if _, err := fmt.Sscanf(fields[2], "%d", &size); err != nil {
			return 0, fmt.Errorf("toolsnap: unparseable blob size in %q", line)
		}
		total += size
	}
	return total, nil
}

func hashLen(format string) int {
	if format == "sha256" {
		return 64
	}
	return 40
}

// gitStdin runs a git command against the bare store with optional
// extra environment and stdin.
func (s *Store) gitStdin(ctx context.Context, extraEnv []string, stdin []byte, args ...string) (string, error) {
	return s.gitStdinDir(ctx, filepath.Dir(s.Dir), extraEnv, stdin, args...)
}

func (s *Store) gitStdinDir(ctx context.Context, dir string, extraEnv []string, stdin []byte, args ...string) (string, error) {
	full := append([]string{"--git-dir", s.Dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = storeGitEnv(extraEnv)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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
