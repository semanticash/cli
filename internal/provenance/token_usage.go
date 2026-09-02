package provenance

import (
	"context"
	"database/sql"
	"path/filepath"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TurnTokenUsage is provider-reported usage for one completed turn.
type TurnTokenUsage struct {
	InputUncached int64 `json:"input_uncached"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheWrite    int64 `json:"cache_write"`
}

type nullableUsage struct {
	inputUncached sql.NullInt64
	output        sql.NullInt64
	cacheRead     sql.NullInt64
	cacheWrite    sql.NullInt64
}

// LoadTurnTokenUsage returns measured usage for one session turn.
// It returns nil when usage is missing or invalid.
func LoadTurnTokenUsage(ctx context.Context, repoPath, provider, providerSessionID, turnID string) (*TurnTokenUsage, error) {
	h, err := sqlstore.Open(ctx, filepath.Join(repoPath, ".semantica", "lineage.db"), sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()
	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	sess, err := resolveProviderSession(ctx, h, repo.RepositoryID, provider, providerSessionID)
	if err != nil {
		return nil, err
	}
	row, err := h.Queries.GetTurnTokenUsage(ctx, sqldb.GetTurnTokenUsageParams{
		SessionID: sess.SessionID,
		TurnID:    sqlstore.NullStr(turnID),
	})
	if err != nil {
		return nil, err
	}
	// Reject malformed rows even when deduplication selects a valid copy.
	if row.InvalidCount != 0 || row.UsageCount == 0 || row.TokensIn < 0 || row.TokensOut < 0 || row.TokensCacheRead < 0 || row.TokensCacheCreate < 0 {
		return nil, nil
	}
	return &TurnTokenUsage{
		InputUncached: row.TokensIn,
		Output:        row.TokensOut,
		CacheRead:     row.TokensCacheRead,
		CacheWrite:    row.TokensCacheCreate,
	}, nil
}

func validTurnTokenUsage(usage *TurnTokenUsage) *TurnTokenUsage {
	if usage == nil || usage.InputUncached < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 {
		return nil
	}
	return usage
}

func nullableTurnTokenUsage(usage *TurnTokenUsage) nullableUsage {
	if usage == nil {
		return nullableUsage{}
	}
	return nullableUsage{
		inputUncached: sql.NullInt64{Int64: usage.InputUncached, Valid: true},
		output:        sql.NullInt64{Int64: usage.Output, Valid: true},
		cacheRead:     sql.NullInt64{Int64: usage.CacheRead, Valid: true},
		cacheWrite:    sql.NullInt64{Int64: usage.CacheWrite, Valid: true},
	}
}

func tokenUsageDiffers(usage *TurnTokenUsage, stored sqldb.ProvenanceManifest) bool {
	if usage == nil {
		return false
	}
	return !stored.TokensIn.Valid || !stored.TokensOut.Valid || !stored.TokensCacheRead.Valid || !stored.TokensCacheCreate.Valid ||
		stored.TokensIn.Int64 != usage.InputUncached || stored.TokensOut.Int64 != usage.Output ||
		stored.TokensCacheRead.Int64 != usage.CacheRead || stored.TokensCacheCreate.Int64 != usage.CacheWrite
}
