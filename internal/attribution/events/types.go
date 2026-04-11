// Package events extracts AI candidate data from attribution event rows.
// It is a pure domain package with no database, blob store, or git dependencies.
// Callers load the input data and pass it in.
package events

// EventRow is a self-contained event for candidate building.
// Callers map source rows into this type and attach any pre-loaded payload
// bytes before calling BuildCandidatesFromRows.
type EventRow struct {
	Provider    string
	Role        string // "assistant", "user", "tool", etc.
	ToolUses    string // raw JSON from the tool_uses column
	PayloadHash string // CAS hash (for diagnostics, not used for loading)
	Payload     []byte // pre-loaded by the caller; nil if unavailable
	Model       string // LLM model name (e.g. "opus 4.6")
}

// Candidates holds the AI-authored text extracted from events.
// Deleted paths from bash `rm` commands are folded into ProviderTouchedFiles
// (they contribute to "AI touched this file", not a separate category).
type Candidates struct {
	AILines              map[string]map[string]struct{} // file -> set of trimmed lines
	ProviderTouchedFiles map[string]string              // file -> provider (file-level, includes deletions)
	FileProvider         map[string]string              // file -> provider (line-level)
	ProviderModel        map[string]string              // provider -> model
}

// EventStats collects diagnostic counters from event processing.
// Each counter is independently meaningful; callers combine EventStats with
// scoring stats to produce the full diagnostics.
type EventStats struct {
	EventsConsidered int
	EventsAssistant  int
	PayloadsLoaded   int
	AIToolEvents     int
}
