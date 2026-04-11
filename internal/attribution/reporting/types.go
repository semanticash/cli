// Package reporting assembles per-file scores into aggregate attribution
// results. It is a pure domain package with no infrastructure dependencies.
package reporting

// FileScoreInput is the narrow input shape for a single file's score data.
type FileScoreInput struct {
	Path            string
	TotalLines      int
	ExactLines      int
	FormattedLines  int
	ModifiedLines   int
	HumanLines      int
	ProviderLines   map[string]int // provider -> AI lines for this file
	DeletedNonBlank int            // deleted non-blank lines (display only, not attributed)
}

// ProviderAttribution holds per-provider AI line counts.
type ProviderAttribution struct {
	Provider string
	Model    string // empty if unknown
	AILines  int
}

// AggregateResult contains the full attribution breakdown produced by
// AggregatePercent. The Percent field is the headline number; the remaining
// fields support richer commit trailers and diagnostics.
type AggregateResult struct {
	Percent        float64
	TotalLines     int
	AILines        int
	ExactLines     int // tier 1: exact trimmed match
	ModifiedLines  int // tier 0 with hunk overlap
	FormattedLines int // tier 2: whitespace-normalized match
	FilesTouched   int // unique files in the diff
	Providers      []ProviderAttribution
}

// CommitResultInput holds the narrow inputs needed to assemble a full
// commit attribution result from scored data and diff metadata.
type CommitResultInput struct {
	FileScores     []FileScoreInput  // one per diff file, in diff order
	FilesCreated   []string          // paths created (from /dev/null)
	FilesDeleted   []string          // paths deleted (to /dev/null)
	TouchedFiles   map[string]bool   // AI-touched file paths (for AI flag on file changes)
	ProviderModels map[string]string // provider -> model
}

// CommitResult is the full attribution breakdown for a single commit,
// produced by BuildCommitResult.
type CommitResult struct {
	AIExactLines     int
	AIFormattedLines int
	AIModifiedLines  int
	AILines          int     // exact + formatted + modified
	HumanLines       int
	TotalLines       int
	AIPercentage     float64 // (exact + formatted + modified) / total * 100
	FilesAITouched   int
	FilesTotal       int // created + edited (excludes deleted)
	FilesCreated     []FileChangeOutput
	FilesEdited      []FileChangeOutput
	FilesDeleted     []FileChangeOutput
	Files            []FileAttributionOutput
	ProviderDetails  []ProviderAttribution
}

// FileAttributionOutput holds per-file attribution scores in the commit result.
type FileAttributionOutput struct {
	Path             string
	AIExactLines     int
	AIFormattedLines int
	AIModifiedLines  int
	HumanLines       int
	TotalLines       int
	DeletedNonBlank  int
	AIPercent        float64 // (exact + formatted + modified) / total * 100
}

// FileChangeOutput records whether a file change was performed by AI.
type FileChangeOutput struct {
	Path string
	AI   bool
}

// CheckpointResultInput holds the narrow inputs for assembling a
// checkpoint-only attribution result (no diff, no line-level scoring).
type CheckpointResultInput struct {
	CheckpointID   string
	TouchedFiles   map[string]bool // AI-touched file paths
	EventStats     EventStatsInput // for diagnostics
}

// EventStatsInput carries event-processing counters into reporting.
type EventStatsInput struct {
	EventsConsidered int
	EventsAssistant  int
	PayloadsLoaded   int
	AIToolEvents     int
}

// CheckpointResult is the attribution result for a checkpoint without
// a linked commit. It reports AI activity but has no line-level scores.
type CheckpointResult struct {
	CheckpointID   string
	FilesAITouched int
	FilesTotal     int
	FilesEdited    []FileChangeOutput
	Diagnostics    CheckpointDiagnostics
}

// CheckpointDiagnostics holds event stats and a diagnostic note for
// checkpoint-only blame results.
type CheckpointDiagnostics struct {
	EventsConsidered int
	EventsAssistant  int
	PayloadsLoaded   int
	AIToolEvents     int
	Note             string
}
