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

// BundleAgentAction is one normalized assistant tool invocation. It
// anchors a finding or trace to what the agent touched without
// claiming why the agent did it.
//
// FilePath and LineRange* are best-effort. Many capture surfaces
// (notably Claude Bash) only record {name:"Bash", file_op:"exec"} in
// tool_uses, and require a payload read to recover anything more.
// Consumers should handle empty FilePath and zero line ranges
// gracefully. If exact scope is unavailable, fall back from line to
// file, then to unspecific activity in the commit window.
//
// Sources records per-field provenance as "field:source" markers
// (e.g. "tool_name:tool_uses", "file_path:payload"). Consumers
// ignore unknown markers; the field exists so the analyzer can show
// which derived values came from which capture surface.
//
// Raw command strings, stdout, stderr, and agent prose are omitted
// deliberately. Downstream consumers only receive this struct.
type BundleAgentAction struct {
	ActionID       string
	TurnID         string
	CheckpointID   string
	TS             int64
	ToolName       string
	FilePath       string
	LineRangeStart int
	LineRangeEnd   int
	Sources        []string
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

// AgentActionLoader loads normalized assistant tool-use records for
// a chronological list of commits. Production implementations may
// inspect provider-specific tool_uses and payload blobs.
type AgentActionLoader interface {
	LoadActionsForCommits(ctx context.Context, commitHashes []string) ([]BundleAgentAction, error)
}

// NoopAgentActionLoader returns no captured actions. Used when no
// action data is available or for tests that don't exercise the
// agent-activity path.
type NoopAgentActionLoader struct{}

func (NoopAgentActionLoader) LoadActionsForCommits(context.Context, []string) ([]BundleAgentAction, error) {
	return nil, nil
}
