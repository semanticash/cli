package implementations

import (
	"context"
	"database/sql"
	"errors"

	"github.com/semanticash/cli/internal/broker"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// attachRule defines a deterministic rule for matching an observation to an
// existing implementation. Rules are evaluated in priority order (lowest
// number = highest priority). First match wins.
type attachRule struct {
	name             string
	priority         int
	canReviveDormant bool
	evaluate         func(ctx context.Context, qtx *impldbgen.Queries, obs processedObs) (string, bool, error)
}

// processedObs is an observation enriched with branch info at reconciliation time.
type processedObs struct {
	impldbgen.Observation
	Branch string // detected by reconciler, not broker
}

// rules is the ordered list of attach rules.
// Priority 2 (commit_trailer) is reserved for v2.
var rules = []attachRule{
	{
		name:             "explicit_link",
		priority:         1,
		canReviveDormant: true,
		evaluate:         matchExplicitLink,
	},
	{
		name:             "session_identity",
		priority:         3,
		canReviveDormant: true,
		evaluate:         matchSessionIdentity,
	},
	{
		name:             "branch_active",
		priority:         5,
		canReviveDormant: false,
		evaluate:         matchBranchActive,
	},
}

// matchExplicitLink: reserved for future use by the link command.
// Observations created by the broker never match this rule; only
// manually-created observations (from `semantica implementations link`)
// would carry an explicit link marker. For now, always returns false.
func matchExplicitLink(_ context.Context, _ *impldbgen.Queries, _ processedObs) (string, bool, error) {
	return "", false, nil
}

// matchSessionIdentity: same provider + same provider_session_id in a
// non-closed implementation, or same provider + observation's
// parent_session_id matches an already-attached session.
func matchSessionIdentity(ctx context.Context, qtx *impldbgen.Queries, obs processedObs) (string, bool, error) {
	// Direct session match
	impl, err := qtx.FindImplementationByProviderSession(ctx, impldbgen.FindImplementationByProviderSessionParams{
		Provider:          obs.Provider,
		ProviderSessionID: obs.ProviderSessionID,
	})
	if err == nil {
		return impl.ImplementationID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	// Parent session tree match
	if obs.ParentSessionID.Valid && obs.ParentSessionID.String != "" {
		impl, err = qtx.FindImplementationByProviderSession(ctx, impldbgen.FindImplementationByProviderSessionParams{
			Provider:          obs.Provider,
			ProviderSessionID: obs.ParentSessionID.String,
		})
		if err == nil {
			return impl.ImplementationID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}

	return "", false, nil
}

// matchBranchActive: same canonical repo + same branch + active implementation.
// Queries implementation_branches (persisted), not observations (ephemeral).
// Most recent active implementation wins. No conflict recorded.
func matchBranchActive(ctx context.Context, qtx *impldbgen.Queries, obs processedObs) (string, bool, error) {
	if obs.Branch == "" {
		return "", false, nil
	}

	canonicalTarget := broker.CanonicalRepoPath(obs.TargetRepoPath)
	impl, err := qtx.FindActiveImplementationByBranch(ctx, impldbgen.FindActiveImplementationByBranchParams{
		CanonicalPath: canonicalTarget,
		Branch:        obs.Branch,
	})
	if err == nil && impl.State == "active" {
		return impl.ImplementationID, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	return "", false, nil
}
