// Package scoring parses unified diffs and scores diff lines against
// AI candidate data. It is a pure domain package with no infrastructure
// dependencies. It receives parsed data and returns scores.
package scoring

// DiffResult holds the parsed output of a unified diff.
type DiffResult struct {
	Files        []FileDiff // all files present in the diff
	FilesCreated []string   // paths created (from /dev/null)
	FilesDeleted []string   // paths deleted (to /dev/null)
}

// FileDiff holds the added lines for a single file in a unified diff,
// grouped into contiguous runs.
type FileDiff struct {
	Path            string       // repo-relative file path
	Groups          []AddedGroup // contiguous groups of added lines
	DeletedNonBlank int          // count of deleted non-blank lines
}

// AddedGroup is a contiguous block of added lines within a diff hunk.
type AddedGroup struct {
	Lines []string // "+" lines with prefix stripped
}

// FileScore holds per-file attribution scores.
//
// ProviderOnlyLines counts lines attributed via provider-touch
// signals alone (the AI session edited the file but no line-level
// payload is available, e.g. Cursor / Copilot / Gemini / Kiro).
// Tracked separately from ModifiedLines so the headline AI% is
// computed from line-overlap evidence only; provider-only lines
// are surfaced in the per-file output but excluded from the
// commit-level percentage to avoid inflating the headline on
// thin evidence.
//
// ProviderLines and ProviderOnlyLinesByProvider mirror that
// split at the per-provider level: a cursor file-touch increments
// only ProviderOnlyLinesByProvider["cursor"], while a claude
// line-level match increments only ProviderLines["claude_code"].
// Aggregating both maps separately keeps the per-provider
// breakdown consistent with the headline split.
type FileScore struct {
	Path                        string
	TotalLines                  int
	ExactLines                  int
	FormattedLines              int
	ModifiedLines               int
	ProviderOnlyLines           int
	HumanLines                  int
	ProviderLines               map[string]int // provider -> line-level AI lines
	ProviderOnlyLinesByProvider map[string]int // provider -> provider-only lines
}

// MatchStats collects match counters from scoring.
// Callers combine these with EventStats from the events package.
type MatchStats struct {
	ExactMatches        int
	NormalizedMatches   int
	ModifiedMatches     int
	ProviderOnlyMatches int
}
