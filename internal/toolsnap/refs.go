package toolsnap

import (
	"context"
	"fmt"
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

// CreateRef atomically creates a tree ref. The ref keeps a pending
// snapshot reachable during store maintenance.
func (s *Store) CreateRef(ctx context.Context, ref, tree string) error {
	zero := strings.Repeat("0", hashLen(s.repo.ObjectFormat))
	if _, err := s.git(ctx, "update-ref", ref, tree, zero); err != nil {
		return fmt.Errorf("toolsnap: create ref %s: %w", ref, err)
	}
	return nil
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
