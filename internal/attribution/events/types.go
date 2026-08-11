// Package events extracts attribution candidates from event rows.
package events

// EventRow contains the event data needed to build candidates.
type EventRow struct {
	Provider    string
	Role        string // "assistant", "user", "tool", etc.
	ToolUses    string // raw JSON from the tool_uses column
	PayloadHash string // CAS hash (for diagnostics, not used for loading)
	Payload     []byte // pre-loaded by the caller; nil if unavailable
	Model       string // LLM model name (e.g. "opus 4.6")
	// Identity and recency used to resolve competing evidence.
	EventID   string
	Ts        int64
	InsertSeq int64
}

// LineStamp is one direct witness for a line; recency compares by
// (Ts, InsertSeq, EventID).
type LineStamp struct {
	Provider  string
	Ts        int64
	InsertSeq int64
	EventID   string
}

// Candidates holds line and file evidence extracted from events.
// LineProviders preserves per-line ownership; FileProvider is the
// fallback for callers without per-line data.
type Candidates struct {
	AILines              map[string]map[string]struct{}            // file -> set of trimmed lines
	LineProviders        map[string]map[string]map[string]struct{} // file -> line -> set of providers that emitted that line
	LineStamps           map[string]map[string][]LineStamp         // file -> line -> direct witnesses
	ProviderTouchedFiles map[string]string                         // file -> provider (file-level, includes deletions)
	FileProvider         map[string]string                         // file -> provider (line-level; last-writer-wins, see LineProviders for per-line breakdown)
	ProviderModel        map[string]string                         // provider -> model
	// InferredDeletions identifies file touches inferred from bash commands.
	InferredDeletions map[string]string // file -> provider
	// ExplicitTouches retains the latest file-edit witness so suppression
	// can restore it after removing an inferred deletion.
	ExplicitTouches map[string]LineStamp
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
