package implementations

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// BranchDetector returns the current git branch for a repo path.
// Returns "" if detection fails or is unavailable.
type BranchDetector func(ctx context.Context, repoPath string) string

// Reconciler processes pending observations and materializes implementations.
type Reconciler struct {
	// DetectBranch returns the current git branch for a repo path.
	// Defaults to git CLI detection if nil. Injected for testing.
	DetectBranch BranchDetector
}

// GitDetectBranch detects the current branch using git CLI.
// Safe to call from the worker (off hot path).
func GitDetectBranch(ctx context.Context, repoPath string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (r *Reconciler) detectBranch(ctx context.Context, repoPath string) string {
	if r.DetectBranch != nil {
		return r.DetectBranch(ctx, repoPath)
	}
	return GitDetectBranch(ctx, repoPath)
}

// Reconcile processes pending observations and materializes implementations.
// Idempotent: safe to call concurrently from multiple workers.
func (r *Reconciler) Reconcile(ctx context.Context, h *impldb.Handle) (*ReconcileResult, error) {
	result := &ReconcileResult{}

	// 1. Mark dormant: active implementations with no activity for DormancyTimeout.
	dormantBefore := time.Now().Add(-DormancyTimeout).UnixMilli()
	dormantResult, err := h.Queries.MarkDormant(ctx, dormantBefore)
	if err != nil {
		return nil, fmt.Errorf("mark dormant: %w", err)
	}
	result.MarkedDormant, _ = dormantResult.RowsAffected()

	// 2. Snapshot deferred observations BEFORE processing pending ones.
	// This ensures only observations deferred in a previous Reconcile pass
	// are picked up, not ones deferred in step 3 below.
	deferred, _ := h.Queries.ListDeferredObservations(ctx, int64(ReconcileBatch/4))

	// 3. Process pending observations (parents first via SQL ordering).
	pending, err := h.Queries.ListPendingObservations(ctx, int64(ReconcileBatch))
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}

	for _, obs := range pending {
		err := r.processObservation(ctx, h, obs)
		if err == errDeferred {
			continue
		}
		if err != nil {
			_ = h.Queries.MarkObservationFailed(ctx, impldbgen.MarkObservationFailedParams{
				LastError:     impldb.NullStr(err.Error()),
				ObservationID: obs.ObservationID,
			})
			result.Errors = append(result.Errors, err)
			continue
		}
		result.Processed++
	}

	// 4. Process deferred observations (snapshotted before step 3).
	for _, obs := range deferred {
		err := r.processObservation(ctx, h, obs)
		if err == errDeferred {
			continue
		}
		if err != nil {
			_ = h.Queries.MarkObservationFailed(ctx, impldbgen.MarkObservationFailedParams{
				LastError:     impldb.NullStr(err.Error()),
				ObservationID: obs.ObservationID,
			})
			continue
		}
		result.DeferredResolved++
	}

	// 5. Retry previously failed observations (up to MaxRetryAttempts).
	retryable, _ := h.Queries.ListRetryableObservations(ctx, impldbgen.ListRetryableObservationsParams{
		ReconcileAttempts: int64(MaxRetryAttempts),
		Limit:             int64(ReconcileBatch / 4),
	})
	for _, obs := range retryable {
		err := r.processObservation(ctx, h, obs)
		if err == errDeferred {
			continue
		}
		if err != nil {
			_ = h.Queries.MarkObservationFailed(ctx, impldbgen.MarkObservationFailedParams{
				LastError:     impldb.NullStr(err.Error()),
				ObservationID: obs.ObservationID,
			})
			continue
		}
		result.Retried++
	}

	return result, nil
}

// errDeferred is a sentinel indicating the observation was deferred, not processed.
var errDeferred = fmt.Errorf("observation deferred")

func (r *Reconciler) processObservation(
	ctx context.Context,
	h *impldb.Handle,
	obs impldbgen.Observation,
) error {
	pObs := processedObs{
		Observation: obs,
		Branch:      r.detectBranch(ctx, obs.TargetRepoPath),
	}

	// The DB is opened with TxImmediate=true, so BeginTx issues
	// BEGIN IMMEDIATE. This acquires a write lock upfront, preventing
	// concurrent workers from both seeing "no owner" and creating
	// duplicate active implementations for the same provider session.
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer impldb.RollbackTx(tx)
	qtx := h.Queries.WithTx(tx)

	// --- Check existing ownership (non-closed) ---
	existingImplID, err := qtx.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
		Provider:          obs.Provider,
		ProviderSessionID: obs.ProviderSessionID,
	})
	if err == nil && existingImplID != "" {
		// Session already belongs to a non-closed implementation.
		// Revive if dormant (session_identity can revive).
		impl, implErr := qtx.GetImplementation(ctx, existingImplID)
		if implErr == nil && impl.State == "dormant" {
			_ = qtx.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
				State:            "active",
				ImplementationID: existingImplID,
			})
		}
		if err := r.updateImplementationState(ctx, qtx, existingImplID, pObs); err != nil {
			return err
		}
		if err := qtx.MarkObservationReconciled(ctx, obs.ObservationID); err != nil {
			return err
		}
		return tx.Commit()
	}

	// --- Parent/child deferral ---
	if obs.ParentSessionID.Valid && obs.ParentSessionID.String != "" {
		_, parentErr := qtx.GetProviderSessionOwner(ctx, impldbgen.GetProviderSessionOwnerParams{
			Provider:          obs.Provider,
			ProviderSessionID: obs.ParentSessionID.String,
		})
		if parentErr != nil && obs.ReconcileAttempts < DeferMaxAttempts {
			if err := qtx.MarkObservationDeferred(ctx, obs.ObservationID); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return errDeferred
		}
	}

	// --- Rule evaluation (all using qtx) ---
	var matchedImplID string
	var matchedRule string

	for _, rule := range rules {
		implID, matched, err := rule.evaluate(ctx, qtx, pObs)
		if err != nil {
			return fmt.Errorf("rule %s: %w", rule.name, err)
		}
		if !matched {
			continue
		}

		impl, err := qtx.GetImplementation(ctx, implID)
		if err != nil {
			continue
		}
		if impl.State == "closed" {
			continue
		}
		if impl.State == "dormant" && !rule.canReviveDormant {
			continue
		}

		matchedImplID = implID
		matchedRule = rule.name
		break
	}

	now := time.Now().UnixMilli()

	if matchedImplID == "" {
		matchedImplID = uuid.NewString()
		matchedRule = "new"
		if err := qtx.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
			ImplementationID: matchedImplID,
			CreatedAt:        now,
			LastActivityAt:   obs.EventTs,
		}); err != nil {
			return fmt.Errorf("insert implementation: %w", err)
		}
	} else {
		impl, _ := qtx.GetImplementation(ctx, matchedImplID)
		if impl.State == "dormant" {
			if err := qtx.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
				State:            "active",
				ImplementationID: matchedImplID,
			}); err != nil {
				return fmt.Errorf("revive dormant: %w", err)
			}
		}
	}

	// --- Determine attach_rule for auditing ---
	attachRule := matchedRule
	if matchedRule == "session_identity" {
		existingRepos, _ := qtx.ListImplementationRepos(ctx, matchedImplID)
		canonicalTarget := broker.CanonicalRepoPath(obs.TargetRepoPath)
		isNewRepo := true
		for _, rr := range existingRepos {
			if rr.CanonicalPath == canonicalTarget {
				isNewRepo = false
				break
			}
		}
		if isNewRepo {
			attachRule = "broker_routing"
		}
	}

	// --- Attach provider session ---
	if err := qtx.InsertProviderSession(ctx, impldbgen.InsertProviderSessionParams{
		ImplementationID:  matchedImplID,
		Provider:          obs.Provider,
		ProviderSessionID: obs.ProviderSessionID,
		SourceProjectPath: obs.SourceProjectPath,
		AttachRule:        attachRule,
		AttachedAt:        now,
	}); err != nil {
		return fmt.Errorf("insert provider session: %w", err)
	}

	// --- Update implementation state ---
	if err := r.updateImplementationState(ctx, qtx, matchedImplID, pObs); err != nil {
		return err
	}

	if err := qtx.MarkObservationReconciled(ctx, obs.ObservationID); err != nil {
		return err
	}
	return tx.Commit()
}

// updateImplementationState handles activity timestamp, repo upsert (with
// origin preservation), branch persistence, and repo-local session identity.
func (r *Reconciler) updateImplementationState(
	ctx context.Context,
	qtx *impldbgen.Queries,
	implID string,
	obs processedObs,
) error {
	if err := qtx.UpdateImplementationActivity(ctx, impldbgen.UpdateImplementationActivityParams{
		LastActivityAt:   obs.EventTs,
		ImplementationID: implID,
	}); err != nil {
		return fmt.Errorf("update activity: %w", err)
	}

	canonicalTarget := broker.CanonicalRepoPath(obs.TargetRepoPath)

	// Role assignment via raw SourceProjectPath.
	role := "downstream"
	if !obs.SourceProjectPath.Valid || obs.SourceProjectPath.String == "" {
		role = "origin"
	} else if broker.PathBelongsToRepo(obs.SourceProjectPath.String, canonicalTarget) {
		role = "origin"
	}

	if err := qtx.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implID,
		CanonicalPath:    canonicalTarget,
		DisplayName:      filepath.Base(canonicalTarget),
		RepoRole:         role,
		FirstSeenAt:      obs.EventTs,
		LastSeenAt:       obs.EventTs,
	}); err != nil {
		return fmt.Errorf("upsert repo: %w", err)
	}

	if obs.Branch != "" {
		if err := qtx.UpsertImplementationBranch(ctx, impldbgen.UpsertImplementationBranchParams{
			ImplementationID: implID,
			CanonicalPath:    canonicalTarget,
			Branch:           obs.Branch,
			FirstSeenAt:      obs.EventTs,
			LastSeenAt:       obs.EventTs,
		}); err != nil {
			return fmt.Errorf("upsert branch: %w", err)
		}
	}

	localSessionID := resolveLocalSessionID(ctx, obs.Observation)
	if localSessionID != "" {
		if err := qtx.UpsertRepoSession(ctx, impldbgen.UpsertRepoSessionParams{
			ImplementationID:  implID,
			Provider:          obs.Provider,
			ProviderSessionID: obs.ProviderSessionID,
			CanonicalPath:     canonicalTarget,
			SessionID:         localSessionID,
			FirstSeenAt:       obs.EventTs,
			LastSeenAt:        obs.EventTs,
		}); err != nil {
			return fmt.Errorf("upsert repo session: %w", err)
		}
	}

	return nil
}

// resolveLocalSessionID opens the target repo's lineage.db and looks up
// the Semantica session_id for this provider_session_id.
// Returns "" if the repo DB is unavailable or the session is not found.
func resolveLocalSessionID(ctx context.Context, obs impldbgen.Observation) string {
	dbPath := filepath.Join(obs.TargetRepoPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return ""
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	// Need repository_id to query by provider session.
	repoRow, err := repoH.Queries.GetRepositoryByRootPath(ctx, obs.TargetRepoPath)
	if err != nil {
		return ""
	}

	session, err := repoH.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID:      repoRow.RepositoryID,
		Provider:          obs.Provider,
		ProviderSessionID: obs.ProviderSessionID,
	})
	if err != nil {
		return ""
	}
	return session.SessionID
}

// AttachCommit links a commit to the implementation that owns the session(s)
// active at commit time. Defaults to one implementation per commit.
// The partial unique index rejects duplicates for non-explicit rules.
func (r *Reconciler) AttachCommit(ctx context.Context, h *impldb.Handle, in AttachCommitInput) error {
	if in.CommitHash == "" {
		return nil
	}

	canonicalPath := broker.CanonicalRepoPath(in.RepoPath)

	// Check if commit is already attached (idempotent).
	_, err := h.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: canonicalPath,
		CommitHash:    in.CommitHash,
	})
	if err == nil {
		return nil // already attached
	}

	// Find sessions linked to this commit's checkpoint via repo's lineage.db.
	sessions := findSessionsForCommit(ctx, in.RepoPath, in.CommitHash)
	if len(sessions) == 0 {
		return nil
	}

	// Look up implementations for these sessions.
	// Count sessions per implementation to pick the one with the most matches.
	implCounts := make(map[string]int)
	for _, sess := range sessions {
		impl, err := h.Queries.FindImplementationByProviderSession(ctx, impldbgen.FindImplementationByProviderSessionParams{
			Provider:          sess.provider,
			ProviderSessionID: sess.providerSessionID,
		})
		if err != nil {
			continue
		}
		implCounts[impl.ImplementationID]++
	}

	if len(implCounts) == 0 {
		return nil
	}

	// Pick the implementation with the most session matches.
	// If tied, skip automatic attachment — the data doesn't distinguish
	// which implementation owns this commit. Ties surface later via
	// `semantica suggest implementations`.
	var bestImplID string
	bestCount := 0
	tied := false
	for id, count := range implCounts {
		if count > bestCount {
			bestCount = count
			bestImplID = id
			tied = false
		} else if count == bestCount {
			tied = true
		}
	}
	if tied {
		return nil
	}

	// Partial unique index rejects if another auto-attach already exists.
	_ = h.Queries.InsertImplementationCommit(ctx, impldbgen.InsertImplementationCommitParams{
		ImplementationID: bestImplID,
		CanonicalPath:    canonicalPath,
		CommitHash:       in.CommitHash,
		AttachedAt:       time.Now().UnixMilli(),
		AttachRule:       "session_identity",
	})

	return nil
}

type sessionRef struct {
	provider          string
	providerSessionID string
}

// findSessionsForCommit opens the repo's lineage.db, finds the checkpoint
// linked to the commit, and returns the sessions associated with it.
func findSessionsForCommit(ctx context.Context, repoPath, commitHash string) []sessionRef {
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		slog.Debug("implementations: cannot open lineage.db for commit attach", "repo", repoPath, "err", err)
		return nil
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	// Find checkpoint linked to this commit.
	commitLink, err := repoH.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		return nil
	}

	// Find sessions linked to that checkpoint.
	sessions, err := repoH.Queries.ListSessionsForCheckpoint(ctx, commitLink.CheckpointID)
	if err != nil {
		return nil
	}

	refs := make([]sessionRef, 0, len(sessions))
	for _, s := range sessions {
		refs = append(refs, sessionRef{
			provider:          s.Provider,
			providerSessionID: s.ProviderSessionID,
		})
	}
	return refs
}
