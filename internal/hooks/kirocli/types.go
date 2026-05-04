package kirocli

import "encoding/json"

// hookPayload is the JSON payload Kiro CLI sends to hooks on stdin.
// SessionID is the Kiro session UUID. Kiro does not include a
// provider tool id in hook payloads.
type hookPayload struct {
	HookEventName     string          `json:"hook_event_name"`
	Cwd               string          `json:"cwd"`
	SessionID         string          `json:"session_id,omitempty"`
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

// fsWriteInput is the tool_input shape for the write tool. The same
// tool name covers create, strReplace, and insert operations,
// distinguished by the Command field.
type fsWriteInput struct {
	Command string `json:"command"` // "create" | "strReplace" | "insert"
	Path    string `json:"path"`
	Content string `json:"content,omitempty"` // for create and insert
	OldStr  string `json:"oldStr,omitempty"`  // for strReplace
	NewStr  string `json:"newStr,omitempty"`  // for strReplace
	Purpose string `json:"__tool_use_purpose,omitempty"`
}

// bashInput is the tool_input shape for the shell tool. Purpose is
// a natural-language hint Kiro CLI passes through with each tool
// call; reusing it as the event summary gives downstream surfaces
// more context than the raw command alone.
type bashInput struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	Purpose    string `json:"__tool_use_purpose,omitempty"`
}

// bashResponseResult is the structured shell result that arrives
// inside an items[].Json entry on the shell response.
type bashResponseResult struct {
	ExitStatus string `json:"exit_status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
}

// bashResponseItem mirrors the shell tool_response.items[] union:
// each item carries either a Json payload (structured shell result)
// or a Text payload (free-form message). Both fields are optional
// so the unmarshaler leaves the unused branch zero-valued.
type bashResponseItem struct {
	Json *bashResponseResult `json:"Json,omitempty"`
	Text string              `json:"Text,omitempty"`
}

// bashResponse is the tool_response shape for shell.
type bashResponse struct {
	Items []bashResponseItem `json:"items"`
}

// subagentInput is the parent-side tool_input shape for AgentCrew.
type subagentInput struct {
	Task    string          `json:"task,omitempty"`
	Mode    string          `json:"mode,omitempty"`
	Stages  []subagentStage `json:"stages,omitempty"`
	Purpose string          `json:"__tool_use_purpose,omitempty"`
}

// subagentStage is one stage in an AgentCrew pipeline.
type subagentStage struct {
	Name           string `json:"name,omitempty"`
	Role           string `json:"role,omitempty"`
	PromptTemplate string `json:"prompt_template,omitempty"`
	Model          string `json:"model,omitempty"`
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

// fsWriteArgs is the parsed args for a write/fs_write tool call as
// stored in the conversation DB at replay time. Both legacy and
// current Kiro CLI shapes are accepted: legacy stores file content
// under file_text, current uses content. Only one will be populated
// per call; ContentText resolves to whichever is non-empty.
type fsWriteArgs struct {
	Command  string `json:"command"`
	Path     string `json:"path"`
	FileText string `json:"file_text,omitempty"`
	Content  string `json:"content,omitempty"`
}

// ContentText returns the new-content string regardless of which
// shape the conversation DB used.
func (a fsWriteArgs) ContentText() string {
	if a.Content != "" {
		return a.Content
	}
	return a.FileText
}

// sidecarOffset is the provider-managed offset file for Kiro CLI.
type sidecarOffset struct {
	LastToolUseID string `json:"last_tool_use_id"`
}
