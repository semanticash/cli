// Package annotations derives conservative, evidence-backed timeline
// annotations for the PR audit development map. It has no database, blob
// store, or git dependencies; callers provide loaded events and a parsed diff.
//
// The detector emits only explicit annotation kinds. Every other observed
// event shape remains ordinary timeline evidence. Unknown or weak signals are
// left unclassified rather than forced into a named kind.
package annotations

// Kind enumerates the conservative v1 annotation categories.
type Kind string

const (
	// KindPossibleRework marks a file where a later agent edit replaced
	// content an earlier agent edit authored within the same window (the
	// agent revised its own earlier output before the commit landed).
	KindPossibleRework Kind = "possible_rework"
	// KindAttemptedRemoved marks a file the agent authored or touched that
	// was then removed by a recognized deletion or by the commit itself.
	KindAttemptedRemoved Kind = "attempted_removed"
)

// AlgorithmVersion identifies the detector revision. Bump it whenever the
// detection logic changes so downstream can treat older annotations as stale.
const AlgorithmVersion = "v1"

// Source identifies where an annotation was produced.
const SourceCLI = "cli"

// Status classifies whether the annotation's supporting step refs can be
// resolved through turn-detail evidence.
type Status string

const (
	// StatusComplete means every supporting step ref carries the turn_id and
	// event_id needed to resolve the step through the turn-detail projection.
	StatusComplete Status = "complete"
	// StatusPartial means at least one supporting step ref is missing the
	// turn_id or event_id needed for resolution.
	StatusPartial Status = "partial"
)

// Event is a single agent event within the commit's attribution window, with
// its assistant payload pre-loaded by the caller. Callers should pass events
// ordered by (TS, EventID) for deterministic temporal comparisons.
//
// The detector reads caller-loaded evidence directly instead of reusing the
// attribution candidate path, because annotations need replaced-content
// evidence that candidate extraction does not retain.
type Event struct {
	EventID        string
	TurnID         string
	Provider       string
	TS             int64  // unix milliseconds
	Role           string // "assistant", "user", "tool", etc.
	ToolUses       string // raw tool_uses JSON (fast pre-filter + provider touches)
	Payload        []byte // pre-loaded assistant payload; nil if unavailable
	ProvenanceHash string // CAS pointer, for supporting step refs
	// ProvenanceBlob is caller-loaded provenance for providers that store
	// replaced content outside the payload. Nil when unavailable; unknown
	// blob shapes are ignored.
	ProvenanceBlob []byte
}

// CommitDiff is the parsed commit under attribution, limited to what the
// detector needs: which files the commit changed and which it deleted.
type CommitDiff struct {
	Files        map[string]bool // repo-relative paths present in the commit
	FilesDeleted map[string]bool // repo-relative paths deleted by the commit
}

// DetectInput bundles everything Detect needs. All data is caller-loaded.
type DetectInput struct {
	CommitSHA string
	RepoRoot  string
	Events    []Event // ordered by (TS, EventID)
	Commit    CommitDiff
}

// StepRef is a structured pointer to a provenance step. It is turn+event-id
// keyed (not a bare hash) so the audit map's evidence drawer can locate the
// step through the turn-detail bundle projection without a generic reader.
type StepRef struct {
	TurnID         string
	EventID        string
	ProvenanceHash string
}

// Anchor pins an annotation onto a node of the development map. Line ranges
// are populated only when the evidence is line-precise; v1 annotations are
// file-precise, so LineStart/LineEnd stay zero.
type Anchor struct {
	EventID   string
	TurnID    string
	CommitSHA string
	FilePath  string
	LineStart int
	LineEnd   int
}

// Annotation is one derived timeline pin. It names the events, turns,
// commit, and file it is anchored to so the UI can render it as an overlay
// on the map rather than as the map itself.
type Annotation struct {
	ID                 string
	Kind               Kind
	FilePath           string
	LineStart          int
	LineEnd            int
	TurnIDs            []string
	Anchors            []Anchor
	SupportingStepRefs []StepRef
	StartedAt          int64
	EndedAt            int64
	Summary            string
	Confidence         float64
	Status             Status
	AlgorithmVersion   string
}
