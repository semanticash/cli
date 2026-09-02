package hooks

import "encoding/json"

// EventType represents a normalized agent lifecycle event.
type EventType int

const (
	PromptSubmitted EventType = iota
	AgentCompleted
	SessionOpened
	SessionClosed
	ContextCompacted
	SubagentSpawned
	SubagentCompleted
	ToolStepCompleted       // state-changing PostToolUse (Write, Edit, Bash)
	SubagentPromptSubmitted // PreToolUse[Agent] prompt event
	IncrementalCapture      // mid-turn trigger to scan transcript from saved offset
	ToolStepStarted         // PreToolUse for tools with paired window capture (Bash)
	AgentResponseCaptured   // final visible assistant response delivered by a hook
)

// HookPhase returns a short stable string for the event's lifecycle point.
// Used by providers to disambiguate event IDs when the same tool_use_id
// appears in both a pre and post hook (e.g., PreToolUse[Agent] and
// PostToolUse[Agent] share a tool_use_id but are different events).
func (t EventType) HookPhase() string {
	switch t {
	case PromptSubmitted:
		return "prompt"
	case SubagentPromptSubmitted:
		return "pre"
	case ToolStepStarted:
		return "pre-step"
	case ToolStepCompleted:
		return "step"
	case SubagentCompleted:
		return "post"
	case AgentCompleted:
		return "stop"
	default:
		return "other"
	}
}

// Event is the provider-agnostic representation of an agent lifecycle event.
// Produced by HookProvider.ParseHookEvent from provider-specific stdin JSON.
type Event struct {
	Type          EventType
	SessionID     string
	TranscriptRef string // path to transcript file
	Prompt        string // user prompt (PromptSubmitted only)
	Model         string // LLM model name
	TokenUsage    *TokenUsage
	Timestamp     int64  // unix ms, from hook payload or time.Now()
	ToolUseID     string // for subagent events and tool steps
	SubagentID    string
	Metadata      map[string]string

	// Step capture fields (ToolStepCompleted, SubagentPromptSubmitted).
	TurnID string // resolved from capture state or set by dispatcher
	CWD    string // session/launch working directory from hook payload
	// EffectiveCWD selects the tool-window repository when a provider supplies
	// a command-specific working directory. An empty value uses CWD.
	EffectiveCWD string
	ToolName     string          // Write, Edit, Bash, Agent, etc.
	ToolInput    json.RawMessage // raw tool_input from hook payload
	ToolResponse json.RawMessage // raw tool_response from hook payload

	// Response is hook-provided final assistant text. Nil means absent; an empty
	// string means the provider returned an empty response.
	Response *string
}

// TokenUsage contains provider-reported totals for one turn.
// TokensIn excludes cached input.
type TokenUsage struct {
	TokensIn          int64 `json:"input_uncached"`
	TokensOut         int64 `json:"output"`
	TokensCacheRead   int64 `json:"cache_read"`
	TokensCacheCreate int64 `json:"cache_write"`
}
