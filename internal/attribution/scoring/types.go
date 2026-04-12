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
type FileScore struct {
	Path           string
	TotalLines     int
	ExactLines     int
	FormattedLines int
	ModifiedLines  int
	HumanLines     int
	ProviderLines  map[string]int // provider -> AI lines for this file
}

// MatchStats collects match counters from scoring.
// Callers combine these with EventStats from the events package.
type MatchStats struct {
	ExactMatches      int
	NormalizedMatches int
	ModifiedMatches   int
}
