package implementations

import (
	"context"
	"fmt"
	"time"

	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// MergeResult reports what was moved.
type MergeResult struct {
	TargetID string
	SourceID string
}

// Merge moves all sessions, commits, repos, and branches from source into
// target. Source is closed. Target keeps its title, state, and created_at.
// Origin role preservation: if target has no origin but source does, the
// origin transfers via the repo upsert logic.
func Merge(ctx context.Context, targetIDInput, sourceIDInput string) (*MergeResult, error) {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	targetID, err := resolveImplID(ctx, h, targetIDInput)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	sourceID, err := resolveImplID(ctx, h, sourceIDInput)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}

	if targetID == sourceID {
		return nil, fmt.Errorf("cannot merge an implementation into itself")
	}

	target, err := h.Queries.GetImplementation(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("target implementation %s not found", targetIDInput)
	}
	source, err := h.Queries.GetImplementation(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("source implementation %s not found", sourceIDInput)
	}

	now := time.Now().UnixMilli()

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer impldb.RollbackTx(tx)
	qtx := h.Queries.WithTx(tx)

	// --- Move provider sessions ---
	// Delete source sessions that already exist in target to avoid PK conflicts.
	targetSessions, err := qtx.ListProviderSessionsForImplementation(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("list target sessions: %w", err)
	}
	targetSessSet := make(map[string]bool)
	for _, s := range targetSessions {
		targetSessSet[s.Provider+"|"+s.ProviderSessionID] = true
	}

	sourceSessions, err := qtx.ListProviderSessionsForImplementation(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list source sessions: %w", err)
	}
	for _, s := range sourceSessions {
		if targetSessSet[s.Provider+"|"+s.ProviderSessionID] {
			if err := qtx.DeleteProviderSession(ctx, impldbgen.DeleteProviderSessionParams{
				ImplementationID:  sourceID,
				Provider:          s.Provider,
				ProviderSessionID: s.ProviderSessionID,
			}); err != nil {
				return nil, fmt.Errorf("delete duplicate provider session: %w", err)
			}
		}
	}
	if err := qtx.MoveProviderSessions(ctx, impldbgen.MoveProviderSessionsParams{
		TargetID: targetID,
		SourceID: sourceID,
	}); err != nil {
		return nil, fmt.Errorf("move provider sessions: %w", err)
	}

	// --- Move repo sessions ---
	// Same PK conflict handling: delete source rows that overlap with target.
	targetRepoSess, err := qtx.ListRepoSessionsForImplementation(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("list target repo sessions: %w", err)
	}
	targetRepoSessSet := make(map[string]bool)
	for _, rs := range targetRepoSess {
		targetRepoSessSet[rs.CanonicalPath+"|"+rs.SessionID] = true
	}

	sourceRepoSess, err := qtx.ListRepoSessionsForImplementation(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list source repo sessions: %w", err)
	}
	for _, rs := range sourceRepoSess {
		if targetRepoSessSet[rs.CanonicalPath+"|"+rs.SessionID] {
			// Delete individually via a direct exec since we don't have a
			// targeted delete query. Use the upsert path instead: skip move
			// for these by deleting from source first.
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM implementation_repo_sessions WHERE implementation_id = ? AND canonical_path = ? AND session_id = ?",
				sourceID, rs.CanonicalPath, rs.SessionID,
			); err != nil {
				return nil, fmt.Errorf("delete duplicate repo session: %w", err)
			}
		}
	}
	if err := qtx.MoveRepoSessions(ctx, impldbgen.MoveRepoSessionsParams{
		TargetID: targetID,
		SourceID: sourceID,
	}); err != nil {
		return nil, fmt.Errorf("move repo sessions: %w", err)
	}

	// --- Move commits ---
	// Delete source commits that conflict with target (same canonical_path + commit_hash).
	targetCommits, err := qtx.ListImplementationCommits(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("list target commits: %w", err)
	}
	targetCommitSet := make(map[string]bool)
	for _, c := range targetCommits {
		targetCommitSet[c.CanonicalPath+"|"+c.CommitHash] = true
	}

	sourceCommits, err := qtx.ListImplementationCommits(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list source commits: %w", err)
	}
	for _, c := range sourceCommits {
		if targetCommitSet[c.CanonicalPath+"|"+c.CommitHash] {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM implementation_commits WHERE implementation_id = ? AND canonical_path = ? AND commit_hash = ?",
				sourceID, c.CanonicalPath, c.CommitHash,
			); err != nil {
				return nil, fmt.Errorf("delete duplicate commit: %w", err)
			}
		}
	}
	if err := qtx.MoveCommits(ctx, impldbgen.MoveCommitsParams{
		TargetID: targetID,
		SourceID: sourceID,
	}); err != nil {
		return nil, fmt.Errorf("move commits: %w", err)
	}

	// --- Merge repos (upsert source repos into target, origin preserved) ---
	sourceRepos, err := qtx.ListImplementationRepos(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list source repos: %w", err)
	}
	for _, r := range sourceRepos {
		if err := qtx.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
			ImplementationID: targetID,
			CanonicalPath:    r.CanonicalPath,
			DisplayName:      r.DisplayName,
			RepoRole:         r.RepoRole,
			FirstSeenAt:      r.FirstSeenAt,
			LastSeenAt:       r.LastSeenAt,
		}); err != nil {
			return nil, fmt.Errorf("upsert repo: %w", err)
		}
	}
	if err := qtx.DeleteReposForImplementation(ctx, sourceID); err != nil {
		return nil, fmt.Errorf("delete source repos: %w", err)
	}

	// --- Merge branches ---
	sourceBranches, err := qtx.ListBranchesForImplementation(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list source branches: %w", err)
	}
	for _, b := range sourceBranches {
		if err := qtx.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
			ImplementationID: targetID,
			CanonicalPath:    b.CanonicalPath,
			Branch:           b.Branch,
			FirstSeenAt:      b.FirstSeenAt,
			LastSeenAt:       b.LastSeenAt,
		}); err != nil {
			return nil, fmt.Errorf("upsert branch: %w", err)
		}
	}
	if err := qtx.DeleteBranchesForImplementation(ctx, sourceID); err != nil {
		return nil, fmt.Errorf("delete source branches: %w", err)
	}

	// --- Update target activity ---
	if source.LastActivityAt > target.LastActivityAt {
		if err := qtx.UpdateImplementationActivity(ctx, impldbgen.UpdateImplementationActivityParams{
			LastActivityAt:   source.LastActivityAt,
			ImplementationID: targetID,
		}); err != nil {
			return nil, fmt.Errorf("update target activity: %w", err)
		}
	}

	// --- Close source ---
	if err := qtx.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State:            "closed",
		ClosedAt:         impldb.NullInt64(now),
		ImplementationID: sourceID,
	}); err != nil {
		return nil, fmt.Errorf("close source: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit merge: %w", err)
	}

	return &MergeResult{TargetID: targetID, SourceID: sourceID}, nil
}
