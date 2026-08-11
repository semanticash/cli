// Package scoring parses unified diffs and scores diff lines against
// AI candidate data. It is a pure domain package with no infrastructure
// dependencies. It receives parsed data and returns scores.
package scoring

// DiffResult holds the parsed output of a unified diff.
type DiffResult struct {
	Files        []FileDiff // all files present in the diff
	FilesCreated []string   // paths created (from /dev/null)
	FilesDeleted []string   // paths deleted (to /dev/null)
	// Complete reports whether the entire diff was scanned.
	Complete bool
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
	// NewStart is the new-file line number of Lines[0], or zero if unknown.
	NewStart int
}

// FileScore holds per-file attribution scores. Provider-only lines are
// reported separately and do not contribute to the headline percentage.
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
	// DeltaExactLines and DeltaFormattedLines identify tool-delta matches.
	DeltaExactLines     int
	DeltaFormattedLines int
	// DeltaAlignmentRefused marks delta evidence that must degrade to touch.
	DeltaAlignmentRefused bool
	// ContestedLines counts lines where multiple evidence candidates
	// competed before winner selection.
	ContestedLines int
}

// MatchStats collects match counters from scoring.
// Callers combine these with EventStats from the events package.
type MatchStats struct {
	ExactMatches           int
	NormalizedMatches      int
	ModifiedMatches        int
	ProviderOnlyMatches    int
	DeltaExactMatches      int
	DeltaNormalizedMatches int
	DeltaAlignmentsRefused int
	ContestedLines         int
}
