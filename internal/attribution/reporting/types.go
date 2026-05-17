// Package reporting assembles per-file scores into aggregate attribution
// results. It is a pure domain package with no infrastructure dependencies.
package reporting

// FileScoreInput is the narrow input shape for a single file's score data.
//
// ProviderOnlyLines counts lines from provider-only files (the AI
// session touched the file but no line-level payload exists).
// Excluded from the headline AI% sum on purpose; surfaced
// separately so callers can render it as a sidecar metric.
//
// ProviderLines and ProviderOnlyLinesByProvider are the
// per-provider counterparts of ExactLines+FormattedLines+
// ModifiedLines and ProviderOnlyLines respectively. They split
// cleanly so a consumer rendering the per-provider breakdown
// can say "claude: N line-level, cursor: M provider-only"
// instead of conflating the two.
type FileScoreInput struct {
	Path                        string
	TotalLines                  int
	ExactLines                  int
	FormattedLines              int
	ModifiedLines               int
	ProviderOnlyLines           int
	HumanLines                  int
	ProviderLines               map[string]int // provider -> line-level AI lines
	ProviderOnlyLinesByProvider map[string]int // provider -> provider-only lines
	DeletedNonBlank             int            // deleted non-blank lines (display only, not attributed)
}

// ProviderAttribution holds per-provider AI line counts.
//
// AILines covers line-level evidence only (exact + formatted +
// modified) to stay consistent with the commit-level headline
// AILines / Percent. ProviderOnlyLines holds the provider-touch-
// only sidecar so consumers can render "N AI lines (M more
// provider-touched)" per agent without conflating the two
// evidence strengths.
type ProviderAttribution struct {
	Provider          string
	Model             string // empty if unknown
	AILines           int    // line-level evidence: exact + formatted + modified
	ProviderOnlyLines int    // provider-touch only; excluded from headline
}

// AggregateResult contains the full attribution breakdown produced by
// AggregatePercent. The Percent field is the headline number; the remaining
// fields support richer commit trailers and diagnostics.
//
// ProviderOnlyLines is the count of lines attributed by provider-touch
// alone (no line-level evidence). Reported here for sidecar rendering;
// deliberately not included in AILines or Percent.
type AggregateResult struct {
	Percent           float64
	TotalLines        int
	AILines           int
	ExactLines        int // tier 1: exact trimmed match
	ModifiedLines     int // tier 0 with hunk overlap
	FormattedLines    int // tier 2: whitespace-normalized match
	ProviderOnlyLines int // provider-touch only, excluded from headline
	FilesTouched      int // unique files in the diff
	Providers         []ProviderAttribution
}

// EvidenceClass describes how a file's AI attribution was determined.
// Internal taxonomy for the evaluation harness and detailed diagnostics.
// User-facing output uses factual notes rather than exposing raw classes.
type EvidenceClass string

const (
	EvidenceExact          EvidenceClass = "exact"           // trimmed exact line match
	EvidenceNormalized     EvidenceClass = "normalized"      // whitespace-normalized match
	EvidenceModified       EvidenceClass = "modified"        // overlap-based modified attribution
	EvidenceProviderTouch  EvidenceClass = "provider_touch"  // explicit file-edit tool event from provider
	EvidenceProviderCoarse EvidenceClass = "provider_coarse" // session-level linkage without direct file-edit event
	EvidenceCarryForward   EvidenceClass = "carry_forward"   // attributed from previous checkpoint window
	EvidenceDeletion       EvidenceClass = "deletion"        // inferred from bash rm / provider deletion
	EvidenceNone           EvidenceClass = "none"            // no AI evidence (human file)
)

// TouchOrigin describes how a file entered the AI-touched set.
// The orchestrator derives this from the candidates produced by the events package.
type TouchOrigin string

const (
	TouchOriginProviderEdit TouchOrigin = "provider_edit" // explicit file-edit tool event (Cursor, Kiro, etc.)
	TouchOriginLineLevel    TouchOrigin = "line_level"    // Claude Edit/Write with payload content
	TouchOriginDeletion     TouchOrigin = "deletion"      // bash rm or provider deletion event
	TouchOriginCoarse       TouchOrigin = "coarse"        // session-level linkage only
)

// CommitResultInput holds the narrow inputs needed to assemble a full
// commit attribution result from scored data and diff metadata.
//
// FileProviders carries the ordered list of providers that own
// matched lines in each AI-attributed file, sorted by line count
// descending. A file edited by Claude (150 lines) and Codex (2 lines)
// produces FileProviders[path] = ["claude_code", "codex"]. Empty or
// missing means human-only file or unknown.
type CommitResultInput struct {
	FileScores        []FileScoreInput       // one per diff file, in diff order
	FilesCreated      []string               // paths created (from /dev/null)
	FilesDeleted      []string               // paths deleted (to /dev/null)
	TouchedFiles      map[string]bool        // AI-touched file paths (for AI flag on file changes)
	ProviderModels    map[string]string      // provider -> model
	FileProviders     map[string][]string    // file -> providers sorted desc by matched line count
	FileTouchOrigins  map[string]TouchOrigin // per-file touch provenance (for evidence classification)
	CarryForwardFiles map[string]bool        // files attributed via carry-forward
}

// CommitResult is the full attribution breakdown for a single commit,
// produced by BuildCommitResult.
//
// AIProviderOnlyLines is reported separately and not summed into
// AILines or AIPercentage. Callers that want a "files touched by AI
// but without line-level evidence" sidecar read from here.
type CommitResult struct {
	AIExactLines        int
	AIFormattedLines    int
	AIModifiedLines     int
	AIProviderOnlyLines int // provider-touch only, excluded from headline
	AILines             int // exact + formatted + modified
	HumanLines          int
	TotalLines          int
	AIPercentage        float64 // (exact + formatted + modified) / total * 100
	FilesAITouched      int
	FilesTotal          int // created + edited (excludes deleted)
	FilesCreated        []FileChangeOutput
	FilesEdited         []FileChangeOutput
	FilesDeleted        []FileChangeOutput
	Files               []FileAttributionOutput
	ProviderDetails     []ProviderAttribution
	Evidence            string // evidence-strength level: "High", "Medium", "Low"
	FallbackCount       int    // number of AI-attributed files with provider-touch or weaker evidence
}

// FileAttributionOutput holds per-file attribution scores in the commit result.
//
// AIProviderOnlyLines is rendered alongside the line-level counts
// but is excluded from AIPercent. PrimaryEvidence will be
// EvidenceProviderTouch (or EvidenceProviderCoarse) for files
// whose only AI evidence is provider-only.
type FileAttributionOutput struct {
	Path                string
	AIExactLines        int
	AIFormattedLines    int
	AIModifiedLines     int
	AIProviderOnlyLines int
	HumanLines          int
	TotalLines          int
	DeletedNonBlank     int
	AIPercent           float64         // (exact + formatted + modified) / total * 100
	PrimaryEvidence     EvidenceClass   // highest-quality evidence for display
	AllEvidence         []EvidenceClass // all contributing evidence classes (for evaluation)
}

// FileChangeOutput records whether a file change was performed by AI.
type FileChangeOutput struct {
	Path      string
	AI        bool
	Providers []string // providers that contributed matched lines, sorted desc by count; empty for human or unknown-provider files
}

// MatchStatsInput carries match counters from scoring into reporting.
type MatchStatsInput struct {
	ExactMatches        int
	NormalizedMatches   int
	ModifiedMatches     int
	ProviderOnlyMatches int
}

// DiagnosticsInput combines event stats, match stats, and the computed
// AI percentage for rendering the diagnostic note.
type DiagnosticsInput struct {
	EventStats EventStatsInput
	MatchStats MatchStatsInput
	AIPercent  float64
}

// CheckpointResultInput holds the narrow inputs for assembling a
// checkpoint-only attribution result (no diff, no line-level scoring).
type CheckpointResultInput struct {
	CheckpointID string
	TouchedFiles map[string]bool // AI-touched file paths
	EventStats   EventStatsInput // for diagnostics
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

// CheckpointDiagnostics holds event stats and diagnostic notes for
// checkpoint-only blame results. Notes carries the pipeline-state
// message wrapped as a slice so the shape matches the commit-path
// AttributionDiagnostics - both CLI display and push paths iterate
// the same slice.
type CheckpointDiagnostics struct {
	EventsConsidered int
	EventsAssistant  int
	PayloadsLoaded   int
	AIToolEvents     int
	Notes            []string
}
