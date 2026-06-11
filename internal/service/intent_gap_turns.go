package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/semanticash/cli/internal/intentgap"
	"github.com/semanticash/cli/internal/redact"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// redactString is replaceable in tests that exercise fail-closed behavior.
var redactString = redact.String

// sqliteTurnLoader reads commit-linked prompts from lineage.db. A missing
// database means no captures; an unreadable database returns an error.
type sqliteTurnLoader struct {
	repoRoot string
}

// newSQLiteTurnLoader binds a turn loader to one repository.
func newSQLiteTurnLoader(repoRoot string) intentgap.TurnLoader {
	return &sqliteTurnLoader{repoRoot: repoRoot}
}

func (l *sqliteTurnLoader) LoadTurnsForCommits(ctx context.Context, commitHashes []string) ([]intentgap.BundleTurn, error) {
	if len(commitHashes) == 0 {
		return nil, nil
	}
	dbPath := filepath.Join(l.repoRoot, ".semantica", "lineage.db")

	// A missing database is a valid empty state; an unreadable one is not.
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		return nil, nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 50 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil, intentgap.ErrLineageUnavailable
	}
	defer func() { _ = sqlstore.Close(h) }()

	var out []intentgap.BundleTurn
	for _, commit := range commitHashes {
		rows, qErr := h.Queries.ListUserPromptsForCommit(ctx, commit)
		if qErr != nil {
			if errors.Is(qErr, context.Canceled) || errors.Is(qErr, context.DeadlineExceeded) {
				return out, qErr
			}
			// Do not treat a failed query as an empty intent history.
			return nil, intentgap.ErrLineageUnavailable
		}
		for _, r := range rows {
			turnID := ""
			if r.TurnID.Valid {
				turnID = r.TurnID.String
			}
			if turnID == "" {
				continue
			}
			rawExcerpt := ""
			if r.Summary.Valid {
				rawExcerpt = r.Summary.String
			}
			// Redact before truncation and hashing so citations match model input.
			redactedExcerpt, redactErr := redactString(rawExcerpt)
			if redactErr != nil {
				// Fail closed rather than analyze an incomplete prompt set.
				return nil, intentgap.ErrRedactionFailed
			}
			truncated := truncateExcerptForBundle(redactedExcerpt)
			out = append(out, intentgap.BundleTurn{
				TurnID:            turnID,
				CommitHash:        commit,
				TS:                r.Ts,
				PromptExcerpt:     truncated,
				PromptExcerptHash: hashExcerpt(truncated),
			})
		}
	}
	return out, nil
}

// truncateExcerptForBundle bounds excerpts while preserving valid UTF-8.
func truncateExcerptForBundle(s string) string {
	const maxExcerpt = 400
	if len(s) <= maxExcerpt {
		return s
	}
	// Back off when the byte limit falls inside a UTF-8 sequence.
	cut := maxExcerpt
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...(truncated)"
}

// hashExcerpt hashes the exact excerpt exposed to the analyzer.
func hashExcerpt(excerpt string) string {
	sum := sha256.Sum256([]byte(excerpt))
	return hex.EncodeToString(sum[:])
}
