package toolsnap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// refPrefix namespaces snapshots inside the isolated store. These refs
// are not visible to operations on the user repository.
const refPrefix = "refs/semantica/tool-windows"

// SnapshotRef returns the private pre-snapshot ref for a tool window.
func SnapshotRef(worktreeID, groupID, toolUseID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/pre", refPrefix,
		sanitizeRefComponent(worktreeID),
		sanitizeRefComponent(groupID),
		sanitizeRefComponent(toolUseID))
}

// GroupPostRef returns the ref that protects a group's post tree.
func GroupPostRef(worktreeID, groupID string) string {
	return fmt.Sprintf("%s/%s/%s/post", refPrefix,
		sanitizeRefComponent(worktreeID),
		sanitizeRefComponent(groupID))
}

// WorktreeID returns the store's worktree identity.
func (s *Store) WorktreeID() string { return s.repo.WorktreeID }

// EnsureRef creates a ref or accepts an identical existing ref.
func (s *Store) EnsureRef(ctx context.Context, ref, tree string) error {
	err := s.CreateRef(ctx, ref, tree)
	if err == nil {
		return nil
	}
	refs, lerr := s.ListRefs(ctx)
	if lerr == nil && refs[ref] == tree {
		return nil
	}
	return err
}

// validRefName accepts only snapshot refs produced by sanitizeRefComponent.
func validRefName(ref string) bool {
	rest, ok := strings.CutPrefix(ref, refPrefix+"/")
	if !ok {
		return false
	}
	for _, comp := range strings.Split(rest, "/") {
		if comp == "" {
			return false
		}
		for _, r := range comp {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return false
			}
		}
	}
	return true
}

// CreateRef atomically publishes a tree ref without replacing an existing ref.
func (s *Store) CreateRef(ctx context.Context, ref, tree string) error {
	if !validRefName(ref) {
		return fmt.Errorf("toolsnap: refusing to create ref outside %s: %s", refPrefix, ref)
	}
	// The caller guarantees that the object exists in the store.
	if !validHash(tree, s.repo.ObjectFormat) {
		return fmt.Errorf("toolsnap: invalid ref target %q", tree)
	}
	// Read errors prevent safe create-if-absent publication.
	packed, err := os.ReadFile(filepath.Join(s.Dir, "packed-refs"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("toolsnap: read packed-refs: %w", err)
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if strings.HasSuffix(line, " "+ref) {
			return fmt.Errorf("toolsnap: create ref %s: already exists (packed)", ref)
		}
	}
	path := filepath.Join(s.Dir, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("toolsnap: create ref parents %s: %w", ref, err)
	}
	// A random suffix avoids collisions with abandoned temp files.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("toolsnap: create ref temp %s: %w", ref, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.WriteString(tree + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: write ref %s: %w", ref, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: sync ref %s: %w", ref, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("toolsnap: close ref %s: %w", ref, err)
	}
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("toolsnap: publish ref %s: %w", ref, err)
	}
	return nil
}

// validHash accepts full lowercase object IDs for the repository format.
func validHash(s, format string) bool {
	if len(s) != hashLen(format) {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// DeleteRef removes ref only when it still points to expectedTree.
func (s *Store) DeleteRef(ctx context.Context, ref, expectedTree string) error {
	if _, err := s.git(ctx, "update-ref", "-d", ref, expectedTree); err != nil {
		return fmt.Errorf("toolsnap: delete ref %s: %w", ref, err)
	}
	return nil
}

// DeleteRefs conditionally deletes refs in one transaction.
// A target conflict leaves the full batch unchanged.
func (s *Store) DeleteRefs(ctx context.Context, refs map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	var in bytes.Buffer
	for ref, tree := range refs {
		if !validRefName(ref) {
			return fmt.Errorf("toolsnap: refusing to delete ref outside %s: %s", refPrefix, ref)
		}
		// Validate targets before writing the NUL-delimited protocol.
		if !validHash(tree, s.repo.ObjectFormat) {
			return fmt.Errorf("toolsnap: invalid delete target %q for ref %s", tree, ref)
		}
		in.WriteString("delete ")
		in.WriteString(ref)
		in.WriteByte(0)
		in.WriteString(tree)
		in.WriteByte(0)
	}
	if _, err := s.gitStdin(ctx, nil, in.Bytes(), "update-ref", "-z", "--stdin"); err != nil {
		return fmt.Errorf("toolsnap: batch delete refs: %w", err)
	}
	return nil
}

// ListRefs returns all snapshot refs and their targets.
func (s *Store) ListRefs(ctx context.Context) (map[string]string, error) {
	out, err := s.git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", refPrefix)
	if err != nil {
		return nil, fmt.Errorf("toolsnap: list refs: %w", err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 {
			refs[parts[0]] = parts[1]
		}
	}
	return refs, nil
}
