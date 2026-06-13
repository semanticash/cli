package intentgap

import (
	"context"
	"errors"
)

// ErrLineageUnavailable means lineage.db exists but cannot be read.
// Missing capture data is represented by an empty result instead.
var ErrLineageUnavailable = errors.New("intentgap: lineage data unavailable")

// ErrRedactionFailed means a captured prompt could not be safely prepared
// for analysis. The analysis fails closed instead of omitting the prompt.
var ErrRedactionFailed = errors.New("intentgap: redaction failed for at least one captured turn")

// BundleTurn is a captured user prompt associated with a commit. The
// analyzer correlates these prompts with the pull request diff to detect:
//
//   - under_impl: a turn whose intent was not fully implemented
//   - unrequested: a region whose intent does not match any captured turn
//   - deferred: a turn whose intent was added then removed (trajectory)
//
// PromptExcerpt and PromptExcerptHash are verified citation anchors.
type BundleTurn struct {
	TurnID            string
	CommitHash        string
	TS                int64
	PromptExcerpt     string
	PromptExcerptHash string
}

// TurnLoader loads captured turns for a chronological list of commits.
type TurnLoader interface {
	LoadTurnsForCommits(ctx context.Context, commitHashes []string) ([]BundleTurn, error)
}

// NoopTurnLoader returns no captured turns.
type NoopTurnLoader struct{}

func (NoopTurnLoader) LoadTurnsForCommits(context.Context, []string) ([]BundleTurn, error) {
	return nil, nil
}
