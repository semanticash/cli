package provenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TurnRecorded reports whether a repository contains an event for a provider turn.
func TurnRecorded(ctx context.Context, repoPath, provider, providerSessionID, turnID string) (bool, error) {
	if repoPath == "" || providerSessionID == "" || turnID == "" {
		return false, nil
	}
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	info, err := os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat lineage db: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("lineage db is not a regular file")
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return false, fmt.Errorf("open lineage db: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("resolve repository: %w", err)
	}
	sess, err := resolveProviderSession(ctx, h, repo.RepositoryID, provider, providerSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve session: %w", err)
	}
	exists, err := h.Queries.TurnEventExists(ctx, sqldb.TurnEventExistsParams{
		SessionID: sess.SessionID,
		TurnID:    sqlstore.NullStr(turnID),
	})
	if err != nil {
		return false, fmt.Errorf("check turn events: %w", err)
	}
	return exists, nil
}

// LoadTurnPrompt returns the prompt reference recorded in a repository.
func LoadTurnPrompt(ctx context.Context, repoPath, provider, providerSessionID, turnID string) (PromptCandidate, error) {
	if repoPath == "" || providerSessionID == "" || turnID == "" {
		return PromptCandidate{}, nil
	}
	h, err := sqlstore.Open(ctx, filepath.Join(repoPath, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		return PromptCandidate{}, fmt.Errorf("open lineage db: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return PromptCandidate{}, fmt.Errorf("resolve repository: %w", err)
	}
	sess, err := resolveProviderSession(ctx, h, repo.RepositoryID, provider, providerSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return PromptCandidate{}, nil
	}
	if err != nil {
		return PromptCandidate{}, fmt.Errorf("resolve session: %w", err)
	}
	prompt := findPromptEvent(ctx, h, sess.SessionID, turnID)
	if prompt == nil {
		return PromptCandidate{}, nil
	}
	return PromptCandidate{EventID: prompt.EventID, Hash: prompt.PayloadHash}, nil
}

func resolveProviderSession(ctx context.Context, h *sqlstore.Handle, repositoryID, provider, providerSessionID string) (sqldb.AgentSession, error) {
	sess, err := h.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID:      repositoryID,
		Provider:          provider,
		ProviderSessionID: providerSessionID,
	})
	if err == nil {
		return sess, nil
	}
	normalized := strings.ReplaceAll(provider, "-", "_")
	if normalized == provider {
		return sqldb.AgentSession{}, err
	}
	return h.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID:      repositoryID,
		Provider:          normalized,
		ProviderSessionID: providerSessionID,
	})
}

func ensurePromptCandidate(ctx context.Context, target, source *blobs.Store, candidate PromptCandidate) (*promptInfo, error) {
	if candidate.EventID == "" && candidate.Hash == "" {
		return nil, nil
	}
	if candidate.EventID == "" || candidate.Hash == "" {
		return nil, fmt.Errorf("incomplete prompt candidate")
	}
	if !target.Exists(candidate.Hash) {
		if source == nil {
			return nil, fmt.Errorf("prompt source unavailable")
		}
		if err := target.Propagate(ctx, candidate.Hash, source); err != nil {
			return nil, fmt.Errorf("propagate prompt object: %w", err)
		}
	}
	return &promptInfo{EventID: candidate.EventID, PayloadHash: candidate.Hash}, nil
}
