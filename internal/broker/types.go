package broker

// RawEvent is a parsed but unrouted event from a provider source.
// Contains all fields needed to write into any target repo's lineage DB.
type RawEvent struct {
	// Identity (content-addressed, stable across reruns).
	EventID   string
	SourceKey string
	Provider  string

	// Parsed fields from the provider's event format.
	Timestamp         int64
	Kind              string // user, assistant, tool_result, etc.
	Role              string // user, assistant, system, tool
	ToolUsesJSON      string // Serialized {"content_types":[...],"tools":[...]}
	Summary           string
	PayloadHash       string // CAS pointer to raw blob in blob store
	TokensIn          int64
	TokensOut         int64
	TokensCacheRead   int64
	TokensCacheCreate int64
	ProviderEventID   string

	// Normalized source position. Provider-specific: line number
	// (JSONL providers), message index (JSON providers). Used for
	// content-addressed event IDs and reconciliation bookmarks.
	SourcePosition int64

	// Routing data - absolute file paths extracted from tool_uses.
	// Used by RouteEvents to determine which repos this event belongs to.
	FilePaths []string

	// Turn and step provenance - links events to their prompt boundary
	// and specific tool invocation for dedup and drill-down.
	TurnID         string // turn that produced this event
	ToolUseID      string // stable provider tool call id
	ToolName       string // Write, Edit, Bash, Agent, etc.
	EventSource    string // "hook" or "transcript"
	ProvenanceHash string // CAS pointer to raw hook payload for backend reconstruction

	// Session context - needed to create/update sessions in target repos.
	ProviderSessionID  string
	ParentSessionID    string // empty if no parent
	SessionStartedAt   int64
	SessionMetaJSON    string
	SourceProjectPath  string // decoded project path for no-path fallback routing
	Model              string // LLM model name (e.g. "opus 4.6", "gemini-2.5-pro")
}

// RepoMatch pairs a registered repo with the events routed to it.
type RepoMatch struct {
	Repo   RegisteredRepo
	Events []RawEvent
}
