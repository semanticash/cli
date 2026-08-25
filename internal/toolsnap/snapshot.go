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

// Snapshot identifies a captured workspace tree and its repository state.
type Snapshot struct {
	TreeHash  string
	HeadHash  string
	DirtyPath []string
}

// HeadAnchor identifies the commit and tree resolved for a capture.
// Private fields prevent callers from constructing inconsistent pairs.
// The zero value represents an unborn branch.
type HeadAnchor struct {
	commit string
	tree   string
}

// CaptureBefore builds a workspace tree from HEAD and current changes.
// Unchanged subtrees retain their existing object IDs.
func (s *Store) CaptureBefore(ctx context.Context) (Snapshot, error) {
	stop := measureStage(ctx, "capture_before", stageAggregate)
	defer stop()
	return s.capture(ctx, nil)
}

// capture builds a snapshot from the store's HEAD or an explicit anchor.
func (s *Store) capture(ctx context.Context, anchor *HeadAnchor) (Snapshot, error) {
	stopHead := measureStage(ctx, "resolve_head", stageLeaf)
	headCommit, headTree, err := s.resolveHead(ctx, anchor)
	stopHead()
	if err != nil {
		return Snapshot{}, err
	}
	stopDirty := measureStage(ctx, "dirty_paths", stageLeaf)
	paths, statusOID, err := dirtyPaths(ctx, s.repo, s.maxPaths())
	stopDirty()
	if err != nil {
		return Snapshot{}, err
	}
	// Reject a snapshot spanning two HEAD states.
	if err := verifyHeadUnmoved(headCommit, statusOID); err != nil {
		return Snapshot{}, err
	}
	// A clean worktree already matches the HEAD tree.
	if len(paths) == 0 {
		return Snapshot{TreeHash: headTree, HeadHash: headCommit}, nil
	}
	tree, err := s.buildTree(ctx, headTree, paths)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{TreeHash: tree, HeadHash: headCommit, DirtyPath: paths}, nil
}

// verifyHeadUnmoved compares resolved HEAD with the status snapshot.
func verifyHeadUnmoved(headCommit, statusOID string) error {
	expected := headCommit
	if expected == "" {
		expected = "(initial)"
	}
	if statusOID == "" || statusOID != expected {
		return &PartialError{
			Reason: ReasonHeadChanged,
			Detail: fmt.Sprintf("HEAD %s but status described %s", expected, statusOID),
		}
	}
	return nil
}

// resolveHead uses the explicit anchor when present. Otherwise it uses the
// store's repository context and falls back to Git when needed.
func (s *Store) resolveHead(ctx context.Context, anchor *HeadAnchor) (commit, tree string, err error) {
	if anchor != nil {
		// Both fields must be set or empty.
		if (anchor.commit == "") != (anchor.tree == "") {
			return "", "", fmt.Errorf("toolsnap: inconsistent HEAD anchor")
		}
		if anchor.tree != "" {
			return anchor.commit, anchor.tree, nil
		}
		empty, eerr := s.emptyTree(ctx)
		return anchor.commit, empty, eerr
	}
	if s.repo.HeadCommit != "" && s.repo.HeadTree != "" {
		return s.repo.HeadCommit, s.repo.HeadTree, nil
	}
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
	stopReadTree := measureStage(ctx, "tree_write", stageLeaf)
	if _, err := s.gitStdin(ctx, env, nil, "read-tree", headTree); err != nil {
		stopReadTree()
		return "", fmt.Errorf("toolsnap: read-tree: %w", err)
	}
	stopReadTree()

	stopHash := measureStage(ctx, "hash", stageLeaf)
	entries, written, err := s.hashWorktreePaths(ctx, paths)
	if err != nil {
		stopHash()
		return "", err
	}
	// Account for the exact blobs written by Git.
	if len(written) > 0 {
		bytesRead, err := s.blobSizeSum(ctx, written)
		if err != nil {
			stopHash()
			return "", err
		}
		if bytesRead > s.maxBytes() {
			stopHash()
			return "", &PartialError{
				Reason: ReasonByteLimit,
				Detail: fmt.Sprintf("hashed content exceeds the %d byte limit", s.maxBytes()),
			}
		}
	}
	stopHash()

	stopTree := measureStage(ctx, "tree_write", stageLeaf)
	defer stopTree()
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

// indexEntry is one update-index record.
type indexEntry struct {
	mode string
	hash string
	path string
}

// gitlinkMode identifies a submodule commit entry.
const gitlinkMode = "160000"

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
		case fi.IsDir():
			// Dirty directory candidates are represented as gitlinks.
			hash, err := gitOutput(ctx, abs, "rev-parse", "HEAD")
			if err != nil {
				return nil, nil, &PartialError{
					Reason: ReasonUnsupportedPath,
					Detail: fmt.Sprintf("%s is a directory without a resolvable HEAD", p),
				}
			}
			typed = append(typed, indexEntry{mode: gitlinkMode, hash: strings.TrimSpace(hash), path: p})
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

	// Gitlinks do not reference blobs in the snapshot store.
	entries := make([]string, 0, len(typed))
	var written []string
	for _, e := range typed {
		entries = append(entries, e.indexInfo())
		if !e.isRemoval() && e.mode != gitlinkMode {
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
	// Keep stdin-based commands consistent with gitOutputEnv on Windows.
	full := append([]string{"-c", "core.longpaths=true", "--git-dir", s.Dir}, args...)
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
