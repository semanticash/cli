package toolsnap

import (
	"context"
	"fmt"
	"strings"
)

// FileChange is one raw tree-level difference between two snapshots.
// Only path, mode, and object identity come from Git; content loading,
// binary detection, and line diffing happen under Semantica's own
// versioned rules so repository diff configuration can never shape
// evidence.
type FileChange struct {
	Path       string
	Op         byte // A, D, M, T (typechange)
	BeforeMode string
	AfterMode  string
	BeforeHash string
	AfterHash  string
}

// DiffTrees compares two snapshot trees with fixed raw plumbing.
// Renames are never inferred; a rename appears as delete plus create.
// Git patch output is never consulted.
func (s *Store) DiffTrees(ctx context.Context, beforeTree, afterTree string) ([]FileChange, error) {
	if beforeTree == afterTree {
		return nil, nil
	}
	out, err := s.git(ctx, "diff-tree",
		"--no-commit-id", "--raw", "-r", "-z", "--no-renames", "--no-abbrev",
		beforeTree, afterTree)
	if err != nil {
		return nil, fmt.Errorf("toolsnap: diff-tree: %w", err)
	}
	return parseRawDiff(out)
}

// parseRawDiff parses NUL-delimited raw diff records:
// ":<oldmode> <newmode> <oldhash> <newhash> <status>\0<path>\0"
func parseRawDiff(out string) ([]FileChange, error) {
	records := strings.Split(out, "\x00")
	var changes []FileChange
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		if rec[0] != ':' {
			return nil, fmt.Errorf("toolsnap: malformed raw diff record: %q", rec)
		}
		fields := strings.Fields(rec[1:])
		if len(fields) != 5 || i+1 >= len(records) {
			return nil, fmt.Errorf("toolsnap: malformed raw diff record: %q", rec)
		}
		status := fields[4]
		if len(status) != 1 || !strings.ContainsAny(status, "ADMT") {
			// --no-renames excludes R/C records. Reject other shapes.
			return nil, fmt.Errorf("toolsnap: unexpected diff status %q", status)
		}
		i++
		changes = append(changes, FileChange{
			Path:       records[i],
			Op:         status[0],
			BeforeMode: fields[0],
			AfterMode:  fields[1],
			BeforeHash: fields[2],
			AfterHash:  fields[3],
		})
	}
	return changes, nil
}

// ReadBlob loads blob content from the store, resolving through the
// repository alternate when the blob belongs to a committed tree.
func (s *Store) ReadBlob(ctx context.Context, hash string) ([]byte, error) {
	out, err := s.gitStdin(ctx, nil, nil, "cat-file", "blob", hash)
	if err != nil {
		return nil, fmt.Errorf("toolsnap: read blob %s: %w", hash, err)
	}
	return []byte(out), nil
}
