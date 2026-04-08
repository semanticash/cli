package implementations

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// LinkSessionInput contains the parameters for linking a session.
type LinkSessionInput struct {
	ImplementationID string
	SessionID        string // Semantica session UUID, prefix, or provider_session_id
	RepoPath         string // optional: repo to search for the session
	Force            bool   // skip confirmation when moving between implementations
}

// LinkSessionResult reports what happened.
type LinkSessionResult struct {
	LinkedProvider    string
	LinkedSessionID   string
	MovedFrom         string // previous implementation ID, if moved
}

// resolvedSession holds all the identity info needed to fully link a session.
type resolvedSession struct {
	provider          string
	providerSessionID string
	canonicalPath     string // repo where found
	localSessionID    string // repo-local Semantica UUID
}

// LinkSession manually attaches a session to an implementation.
func LinkSession(ctx context.Context, in LinkSessionInput) (*LinkSessionResult, error) {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	targetID, err := resolveImplID(ctx, h, in.ImplementationID)
	if err != nil {
		return nil, err
	}
	if _, err := h.Queries.GetImplementation(ctx, targetID); err != nil {
		return nil, fmt.Errorf("implementation %s not found", in.ImplementationID)
	}

	// Step 1: Try to find by repo-local Semantica session UUID in existing
	// implementation_repo_sessions.
	impls, err := h.Queries.FindImplementationsByLocalSession(ctx, in.SessionID)
	if err == nil && len(impls) > 0 {
		allRepoSess, _ := h.Queries.ListRepoSessionsForImplementation(ctx, impls[0].ImplementationID)
		for _, rs := range allRepoSess {
			if rs.SessionID == in.SessionID {
				resolved := resolvedSession{
					provider:          rs.Provider,
					providerSessionID: rs.ProviderSessionID,
					canonicalPath:     rs.CanonicalPath,
					localSessionID:    rs.SessionID,
				}
				return linkResolvedSession(ctx, h, targetID, resolved, in.Force)
			}
		}
	}

	// Step 2: Search repo lineage DBs by Semantica session UUID/prefix
	// OR provider_session_id match.
	resolved, err := resolveSessionFromRepos(ctx, in.SessionID, in.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("session %q not found: %w", in.SessionID, err)
	}

	return linkResolvedSession(ctx, h, targetID, resolved, in.Force)
}

// linkResolvedSession performs the actual link inside a transaction: moves
// provider session + repo sessions from old implementation (if force-moving),
// inserts into target, upserts repo membership, updates activity.
func linkResolvedSession(ctx context.Context, h *impldb.Handle, targetID string, resolved resolvedSession, force bool) (*LinkSessionResult, error) {
	now := time.Now().UnixMilli()
	result := &LinkSessionResult{
		LinkedProvider:  resolved.provider,
		LinkedSessionID: resolved.providerSessionID,
	}

	// Pre-flight check: does an implementation already own this session?
	existingOwner, err := h.Queries.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
		Provider:          resolved.provider,
		ProviderSessionID: resolved.providerSessionID,
	})
	if err == nil && existingOwner != "" {
		if existingOwner == targetID {
			// Already linked to target. Backfill any missing repo slices
			// so the result is convergent, not just duplicate-safe.
			if err := backfillRepoSlices(ctx, h, targetID, resolved); err != nil {
				return nil, fmt.Errorf("backfill repo slices: %w", err)
			}
			return result, nil
		}
		if !force {
			return nil, fmt.Errorf("session already belongs to implementation %s (use --force to move)",
				existingOwner[:minLen(len(existingOwner), 8)])
		}
		result.MovedFrom = existingOwner
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer impldb.RollbackTx(tx)
	qtx := h.Queries.WithTx(tx)

	// If force-moving, collect all repo sessions and their roles from the old
	// implementation before deleting, so we can re-insert them into the target
	// with the correct role (preserving cross-repo coverage and role semantics).
	var movedRepoSessions []impldbgen.ImplementationRepoSession
	oldRepoRoles := make(map[string]string) // canonical_path → role
	if result.MovedFrom != "" {
		allOldRepoSess, _ := qtx.ListRepoSessionsForImplementation(ctx, result.MovedFrom)
		for _, rs := range allOldRepoSess {
			if rs.Provider == resolved.provider && rs.ProviderSessionID == resolved.providerSessionID {
				movedRepoSessions = append(movedRepoSessions, rs)
			}
		}
		oldRepos, _ := qtx.ListImplementationRepos(ctx, result.MovedFrom)
		for _, r := range oldRepos {
			oldRepoRoles[r.CanonicalPath] = r.RepoRole
		}

		if err := qtx.DeleteProviderSession(ctx, impldbgen.DeleteProviderSessionParams{
			ImplementationID:  result.MovedFrom,
			Provider:          resolved.provider,
			ProviderSessionID: resolved.providerSessionID,
		}); err != nil {
			return nil, fmt.Errorf("delete old provider session: %w", err)
		}
		if err := qtx.DeleteRepoSessionsByProviderSession(ctx, impldbgen.DeleteRepoSessionsByProviderSessionParams{
			ImplementationID:  result.MovedFrom,
			Provider:          resolved.provider,
			ProviderSessionID: resolved.providerSessionID,
		}); err != nil {
			return nil, fmt.Errorf("delete old repo sessions: %w", err)
		}
		// Delete branch rows for each moved session's repos from the source.
		// Branch rows are repo-scoped rather than session-scoped.
		// Clear them for repos touched by the moved session and let future
		// reconciliation restore any branch data that still applies.
		movedRepoPaths := make(map[string]bool)
		for _, rs := range movedRepoSessions {
			movedRepoPaths[rs.CanonicalPath] = true
		}
		for repoPath := range movedRepoPaths {
			if err := qtx.DeleteBranchesForRepo(ctx, impldbgen.DeleteBranchesForRepoParams{
				ImplementationID: result.MovedFrom,
				CanonicalPath:    repoPath,
			}); err != nil {
				return nil, fmt.Errorf("delete branches for moved repo: %w", err)
			}
		}
		// Remove repos from old implementation that no longer have any data.
		if err := qtx.DeleteOrphanedRepos(ctx, result.MovedFrom); err != nil {
			return nil, fmt.Errorf("clean orphaned repos: %w", err)
		}
	}

	// Insert provider session into target.
	if err := qtx.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  targetID,
		Provider:          resolved.provider,
		ProviderSessionID: resolved.providerSessionID,
		AttachRule:        "explicit_link",
		AttachedAt:        now,
	}); err != nil {
		return nil, fmt.Errorf("insert provider session: %w", err)
	}

	// Re-insert all repo sessions from the old implementation into the target,
	// preserving cross-repo coverage.
	for _, rs := range movedRepoSessions {
		if err := qtx.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
			ImplementationID:  targetID,
			Provider:          rs.Provider,
			ProviderSessionID: rs.ProviderSessionID,
			CanonicalPath:     rs.CanonicalPath,
			SessionID:         rs.SessionID,
			FirstSeenAt:       rs.FirstSeenAt,
			LastSeenAt:        rs.LastSeenAt,
		}); err != nil {
			return nil, fmt.Errorf("move repo session: %w", err)
		}

		// Carry the role from the old implementation to preserve repo semantics.
		role := oldRepoRoles[rs.CanonicalPath]
		if role == "" {
			role = "related"
		}
		if err := qtx.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
			ImplementationID: targetID,
			CanonicalPath:    rs.CanonicalPath,
			DisplayName:      filepath.Base(rs.CanonicalPath),
			RepoRole:         role,
			FirstSeenAt:      rs.FirstSeenAt,
			LastSeenAt:       rs.LastSeenAt,
		}); err != nil {
			return nil, fmt.Errorf("upsert repo for moved session: %w", err)
		}

		// Upsert current branch so the target is visible to branch_active.
		if branch := GitDetectBranch(ctx, rs.CanonicalPath); branch != "" {
			if err := qtx.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
				ImplementationID: targetID,
				CanonicalPath:    rs.CanonicalPath,
				Branch:           branch,
				FirstSeenAt:      now,
				LastSeenAt:       now,
			}); err != nil {
				return nil, fmt.Errorf("upsert branch for moved session: %w", err)
			}
		}
	}

	// If this is a fresh link (not a move), scan all registered repos for
	// this provider session and insert all repo-local rows. The resolved
	// session is always inserted as a baseline even if the registry scan
	// finds nothing (e.g., repo not registered in broker).
	if len(movedRepoSessions) == 0 {
		allRepoSlices := findAllRepoSlices(ctx, resolved.provider, resolved.providerSessionID)

		// Ensure the resolved session is included even if its repo is not
		// in the broker registry.
		if resolved.localSessionID != "" && resolved.canonicalPath != "" {
			found := false
			for _, rs := range allRepoSlices {
				if rs.canonicalPath == resolved.canonicalPath && rs.localSessionID == resolved.localSessionID {
					found = true
					break
				}
			}
			if !found {
				allRepoSlices = append(allRepoSlices, repoSlice{
					canonicalPath:  resolved.canonicalPath,
					localSessionID: resolved.localSessionID,
				})
			}
		}

		for _, rs := range allRepoSlices {
			if err := qtx.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
				ImplementationID:  targetID,
				Provider:          resolved.provider,
				ProviderSessionID: resolved.providerSessionID,
				CanonicalPath:     rs.canonicalPath,
				SessionID:         rs.localSessionID,
				FirstSeenAt:       now,
				LastSeenAt:        now,
			}); err != nil {
				return nil, fmt.Errorf("upsert repo session: %w", err)
			}

			if err := qtx.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
				ImplementationID: targetID,
				CanonicalPath:    rs.canonicalPath,
				DisplayName:      filepath.Base(rs.canonicalPath),
				RepoRole:         "related",
				FirstSeenAt:      now,
				LastSeenAt:       now,
			}); err != nil {
				return nil, fmt.Errorf("upsert repo: %w", err)
			}

			// Upsert current branch so the target is visible to branch_active.
			if branch := GitDetectBranch(ctx, rs.canonicalPath); branch != "" {
				if err := qtx.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
					ImplementationID: targetID,
					CanonicalPath:    rs.canonicalPath,
					Branch:           branch,
					FirstSeenAt:      now,
					LastSeenAt:       now,
				}); err != nil {
					return nil, fmt.Errorf("upsert branch: %w", err)
				}
			}
		}
	}

	if err := qtx.UpdateImplementationActivity(ctx, impldbgen.UpdateImplementationActivityParams{
		LastActivityAt:   now,
		ImplementationID: targetID,
	}); err != nil {
		return nil, fmt.Errorf("update activity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit link: %w", err)
	}

	return result, nil
}

// backfillRepoSlices scans all repos for the provider session and upserts
// any missing repo-local session rows + repo membership into the target.
// Always includes the resolved session as a baseline even if the repo isn't
// in the broker registry.
func backfillRepoSlices(ctx context.Context, h *impldb.Handle, targetID string, resolved resolvedSession) error {
	now := time.Now().UnixMilli()
	allSlices := findAllRepoSlices(ctx, resolved.provider, resolved.providerSessionID)

	if resolved.localSessionID != "" && resolved.canonicalPath != "" {
		found := false
		for _, rs := range allSlices {
			if rs.canonicalPath == resolved.canonicalPath && rs.localSessionID == resolved.localSessionID {
				found = true
				break
			}
		}
		if !found {
			allSlices = append(allSlices, repoSlice{
				canonicalPath:  resolved.canonicalPath,
				localSessionID: resolved.localSessionID,
			})
		}
	}

	for _, rs := range allSlices {
		if err := h.Queries.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
			ImplementationID:  targetID,
			Provider:          resolved.provider,
			ProviderSessionID: resolved.providerSessionID,
			CanonicalPath:     rs.canonicalPath,
			SessionID:         rs.localSessionID,
			FirstSeenAt:       now,
			LastSeenAt:        now,
		}); err != nil {
			return err
		}

		if err := h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
			ImplementationID: targetID,
			CanonicalPath:    rs.canonicalPath,
			DisplayName:      filepath.Base(rs.canonicalPath),
			RepoRole:         "related",
			FirstSeenAt:      now,
			LastSeenAt:       now,
		}); err != nil {
			return err
		}

		if branch := GitDetectBranch(ctx, rs.canonicalPath); branch != "" {
			if err := h.Queries.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
				ImplementationID: targetID,
				CanonicalPath:    rs.canonicalPath,
				Branch:           branch,
				FirstSeenAt:      now,
				LastSeenAt:       now,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// repoSlice is a repo-local session identity found during cross-repo scan.
type repoSlice struct {
	canonicalPath  string
	localSessionID string
}

// findAllRepoSlices scans all registered repos for a provider session and
// returns the repo-local session identity for each repo that has it.
func findAllRepoSlices(ctx context.Context, provider, providerSessionID string) []repoSlice {
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return nil
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return nil
	}
	defer func() { _ = broker.Close(bh) }()

	repos, _ := broker.ListActiveRepos(ctx, bh)
	var slices []repoSlice

	for _, r := range repos {
		dbPath := filepath.Join(r.Path, ".semantica", "lineage.db")
		repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
		if err != nil {
			continue
		}

		repoRow, err := repoH.Queries.GetRepositoryByRootPath(ctx, r.Path)
		if err != nil {
			_ = sqlstore.Close(repoH)
			continue
		}

		sess, err := repoH.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
			RepositoryID:      repoRow.RepositoryID,
			Provider:          provider,
			ProviderSessionID: providerSessionID,
		})
		_ = sqlstore.Close(repoH)
		if err != nil {
			continue
		}

		slices = append(slices, repoSlice{
			canonicalPath:  broker.CanonicalRepoPath(r.Path),
			localSessionID: sess.SessionID,
		})
	}

	return slices
}

// resolveSessionFromRepos searches repo lineage DBs for a session by
// Semantica UUID/prefix OR provider_session_id match.
// Ambiguity errors are propagated immediately rather than swallowed.
func resolveSessionFromRepos(ctx context.Context, sessionID, repoPath string) (resolvedSession, error) {
	// If a specific repo is given, search there first.
	if repoPath != "" {
		resolved, err := searchRepoForSession(ctx, repoPath, sessionID)
		if err == nil {
			return resolved, nil
		}
		// Propagate ambiguity - do not fall through to other repos.
		if isAmbiguityError(err) {
			return resolvedSession{}, err
		}
	}

	// Fan out to all registered repos.
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return resolvedSession{}, fmt.Errorf("no broker registry: %w", err)
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return resolvedSession{}, fmt.Errorf("open broker: %w", err)
	}
	defer func() { _ = broker.Close(bh) }()

	repos, _ := broker.ListActiveRepos(ctx, bh)
	for _, r := range repos {
		if r.Path == repoPath {
			continue // already searched
		}
		resolved, err := searchRepoForSession(ctx, r.Path, sessionID)
		if err == nil {
			return resolved, nil
		}
		if isAmbiguityError(err) {
			return resolvedSession{}, err
		}
	}

	return resolvedSession{}, fmt.Errorf("not found in any registered repo")
}

// isAmbiguityError returns true if the error represents an ambiguous match
// that should not be retried on other repos.
func isAmbiguityError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ambiguous")
}

// searchRepoForSession opens a repo's lineage.db and searches for a session
// by Semantica UUID/prefix first, then by provider_session_id match.
func searchRepoForSession(ctx context.Context, repoPath, sessionID string) (resolvedSession, error) {
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return resolvedSession{}, err
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	repoRow, err := repoH.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return resolvedSession{}, err
	}
	canonicalPath := broker.CanonicalRepoPath(repoPath)

	// Try exact Semantica session UUID.
	if sess, err := repoH.Queries.GetAgentSessionByID(ctx, sessionID); err == nil {
		return resolvedSession{
			provider:          sess.Provider,
			providerSessionID: sess.ProviderSessionID,
			canonicalPath:     canonicalPath,
			localSessionID:    sess.SessionID,
		}, nil
	}

	// Try Semantica session UUID prefix.
	if fullID, err := sqlstore.ResolveSessionID(ctx, repoH.Queries, repoRow.RepositoryID, sessionID); err == nil {
		if sess, err := repoH.Queries.GetAgentSessionByID(ctx, fullID); err == nil {
			return resolvedSession{
				provider:          sess.Provider,
				providerSessionID: sess.ProviderSessionID,
				canonicalPath:     canonicalPath,
				localSessionID:    sess.SessionID,
			}, nil
		}
	}

	// Try provider_session_id match (across all providers in this repo).
	// If multiple providers share the same ID, surface ambiguity.
	matches, err := repoH.Queries.ListAgentSessionsByProviderSessionID(ctx, sqldb.ListAgentSessionsByProviderSessionIDParams{
		RepositoryID:      repoRow.RepositoryID,
		ProviderSessionID: sessionID,
	})
	if err == nil {
		switch len(matches) {
		case 1:
			return resolvedSession{
				provider:          matches[0].Provider,
				providerSessionID: matches[0].ProviderSessionID,
				canonicalPath:     canonicalPath,
				localSessionID:    matches[0].SessionID,
			}, nil
		case 0:
			// fall through
		default:
			return resolvedSession{}, fmt.Errorf(
				"ambiguous provider_session_id %q in %s: matches %d providers",
				sessionID, repoPath, len(matches))
		}
	}

	return resolvedSession{}, fmt.Errorf("session not found in %s", repoPath)
}

// LinkCommitInput contains the parameters for linking a commit.
type LinkCommitInput struct {
	ImplementationID string
	CommitHash       string
	RepoPath         string // repo containing the commit
}

// LinkCommit manually attaches a commit to an implementation.
// Uses attach_rule="explicit_link" which bypasses the partial unique index.
func LinkCommit(ctx context.Context, in LinkCommitInput) error {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	targetID, err := resolveImplID(ctx, h, in.ImplementationID)
	if err != nil {
		return err
	}

	if _, err := h.Queries.GetImplementation(ctx, targetID); err != nil {
		return fmt.Errorf("implementation %s not found", in.ImplementationID)
	}

	canonicalPath := broker.CanonicalRepoPath(in.RepoPath)
	now := time.Now().UnixMilli()

	// Verify the commit exists in the repo.
	dbPath := filepath.Join(in.RepoPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return fmt.Errorf("cannot open repo %s: %w", in.RepoPath, err)
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	if _, err := repoH.Queries.GetCommitLinkByCommitHash(ctx, in.CommitHash); err != nil {
		return fmt.Errorf("commit %s not found in repo %s", in.CommitHash, in.RepoPath)
	}

	return h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: targetID,
		CanonicalPath:    canonicalPath,
		CommitHash:       in.CommitHash,
		AttachedAt:       now,
		AttachRule:       "explicit_link",
	})
}
