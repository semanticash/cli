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
)

// HookPhase returns a short stable string for the event's lifecycle phase.
// Used by providers to disambiguate event IDs when the same tool_use_id
// appears in both a pre and post hook (e.g., PreToolUse[Agent] and
// PostToolUse[Agent] share a tool_use_id but are different events).
func (t EventType) HookPhase() string {
	switch t {
	case PromptSubmitted:
		return "prompt"
	case SubagentPromptSubmitted:
		return "pre"
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
	Timestamp     int64  // unix ms, from hook payload or time.Now()
	ToolUseID     string // for subagent events and tool steps
	SubagentID    string
	Metadata      map[string]string

	// Step capture fields (ToolStepCompleted, SubagentPromptSubmitted).
	TurnID       string          // resolved from capture state or set by dispatcher
	CWD          string          // working directory from hook payload
	ToolName     string          // Write, Edit, Bash, Agent, etc.
	ToolInput    json.RawMessage // raw tool_input from hook payload
	ToolResponse json.RawMessage // raw tool_response from hook payload
}
