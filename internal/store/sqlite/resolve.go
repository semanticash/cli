package sqlite

import (
	"context"
	"fmt"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// ResolveCheckpointID resolves a (possibly abbreviated) checkpoint ID to its
// full UUID. If id is already 36 characters it is treated as an exact match
// via GetCheckpointByID. Otherwise, the prefix is matched against the
// repository's checkpoints; an error is returned when zero or more than one
// checkpoint matches.
func ResolveCheckpointID(ctx context.Context, q *sqldb.Queries, repoID, id string) (string, error) {
	// Fast path: full UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
	if len(id) == 36 {
		if _, err := q.GetCheckpointByID(ctx, id); err != nil {
			return "", fmt.Errorf("checkpoint not found: %s", id)
		}
		return id, nil
	}

	matches, err := q.ResolveCheckpointByPrefix(ctx, sqldb.ResolveCheckpointByPrefixParams{
		CheckpointID: id + "%",
		RepositoryID: repoID,
	})
	if err != nil {
		return "", fmt.Errorf("resolve checkpoint prefix: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("checkpoint not found: %s", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("checkpoint prefix %q is ambiguous, provide more characters", id)
	}
}

// ResolveSessionID resolves a (possibly abbreviated) session ID to its full
// UUID. Mirrors ResolveCheckpointID but queries agent_sessions.
func ResolveSessionID(ctx context.Context, q *sqldb.Queries, repoID, id string) (string, error) {
	// Fast path: full UUID.
	if len(id) == 36 {
		sess, err := q.GetAgentSessionByID(ctx, id)
		if err != nil {
			return "", fmt.Errorf("session not found: %s", id)
		}
		if sess.RepositoryID != repoID {
			return "", fmt.Errorf("session not found: %s", id)
		}
		return id, nil
	}

	matches, err := q.ResolveSessionByPrefix(ctx, sqldb.ResolveSessionByPrefixParams{
		SessionID:    id + "%",
		RepositoryID: repoID,
	})
	if err != nil {
		return "", fmt.Errorf("resolve session prefix: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("session not found: %s", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("session prefix %q is ambiguous, provide more characters", id)
	}
}
