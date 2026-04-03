package kirocli

import "encoding/json"

// hookPayload is the JSON payload Kiro CLI sends to hooks on stdin.
type hookPayload struct {
	HookEventName     string          `json:"hook_event_name"`
	Cwd               string          `json:"cwd"`
	Prompt            string          `json:"prompt,omitempty"`
	ToolName          string          `json:"tool_name,omitempty"`
	ToolInput         json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse      json.RawMessage `json:"tool_response,omitempty"`
	AssistantResponse string          `json:"assistant_response,omitempty"`
}

// subagentTools maps Kiro CLI tool names for subagent delegation.
var subagentTools = map[string]bool{
	"subagent":     true,
	"use_subagent": true,
	"delegate":     true,
}

// fsWriteInput is the tool_input shape for fs_write from Kiro CLI hooks.
type fsWriteInput struct {
	Command  string `json:"command"`  // "create" or "str_replace"
	Path     string `json:"path"`
	FileText string `json:"file_text,omitempty"` // for create
	OldStr   string `json:"old_str,omitempty"`   // for str_replace
	NewStr   string `json:"new_str,omitempty"`   // for str_replace
}

// bashInput is the tool_input shape for execute_bash from Kiro CLI hooks.
type bashInput struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
}

// bashResponseResult is a single result entry in execute_bash tool_response.
type bashResponseResult struct {
	ExitStatus string `json:"exit_status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
}

// bashResponse is the tool_response shape for execute_bash.
type bashResponse struct {
	Success bool                 `json:"success"`
	Result  []bashResponseResult `json:"result"`
}

// subagentInput is the tool_input shape for use_subagent/delegate.
type subagentInput struct {
	Prompt string `json:"prompt,omitempty"`
	Task   string `json:"task,omitempty"`
}

// conversationValue is the parsed JSON stored in conversations_v2.value.
type conversationValue struct {
	ConversationID string         `json:"conversation_id"`
	History        []historyEntry `json:"history"`
}

// historyEntry is a single user+assistant turn in the conversation.
type historyEntry struct {
	User      json.RawMessage `json:"user"`
	Assistant json.RawMessage `json:"assistant"`
}

// assistantToolUse is the ToolUse variant of the assistant field.
type assistantToolUse struct {
	ToolUse *toolUseBlock `json:"ToolUse,omitempty"`
}

type toolUseBlock struct {
	MessageID string    `json:"message_id"`
	ToolUses  []toolUse `json:"tool_uses"`
}

type toolUse struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// fsWriteArgs is the parsed args for an fs_write tool call (transcript replay).
type fsWriteArgs struct {
	Command  string `json:"command"` // "create" or "edit"
	Path     string `json:"path"`
	FileText string `json:"file_text"`
}

// sidecarOffset is the provider-managed offset file for Kiro CLI.
type sidecarOffset struct {
	LastToolUseID string `json:"last_tool_use_id"`
}
