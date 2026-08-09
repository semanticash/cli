package toolsnap

import (
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

// validRefName enforces the snapshot namespace at the operation
// itself. Components after the fixed prefix must use the safe
// alphabet sanitizeRefComponent produces: this excludes dot-dot,
// backslashes (a separator on Windows via filepath.FromSlash), and
// every other path metacharacter, so a crafted ref cannot traverse
// outside the store.
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

// CreateRef atomically creates a tree ref. The ref keeps a pending
// snapshot reachable during store maintenance.
//
// Publication is crash-safe without a git spawn: the content is fully
// written and synced to a temporary file, then linked to the final
// name. Link is atomic no-replace, so a partially written or empty
// ref can never appear under the final name and a duplicate create
// fails. packed-refs is consulted first so a packed ref cannot be
// silently shadowed; store maintenance must not pack active snapshot
// refs.
func (s *Store) CreateRef(ctx context.Context, ref, tree string) error {
	if !validRefName(ref) {
		return fmt.Errorf("toolsnap: refusing to create ref outside %s: %s", refPrefix, ref)
	}
	// git update-ref validated the target; a direct write must do the
	// same for form. Object existence is the caller's guarantee: the
	// hash comes from write-tree in the same process.
	if !validHash(tree, s.repo.ObjectFormat) {
		return fmt.Errorf("toolsnap: invalid ref target %q", tree)
	}
	// Only a missing packed-refs file may be ignored: publishing
	// blind on a read error would weaken create-if-absent.
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
	// A random suffix keeps crash remnants genuinely ignorable; a
	// PID-only name could collide after PID reuse.
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

// validHash reports whether s is a full lowercase hex object name for
// the given format, the only form Semantica's own tree building
// produces.
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
