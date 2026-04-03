package kiro

import "encoding/json"

// SessionIndex is the top-level sessions.json structure.
type SessionIndex struct {
	SessionID          string `json:"sessionId"`
	Title              string `json:"title"`
	DateCreated        string `json:"dateCreated"`
	WorkspaceDirectory string `json:"workspaceDirectory"`
}

// SessionHistory is the full session JSON file (<sessionId>.json).
type SessionHistory struct {
	History            []HistoryEntry `json:"history"`
	SessionID          string         `json:"sessionId"`
	WorkspaceDirectory string         `json:"workspaceDirectory"`
}

// HistoryEntry is a single message in the session history.
type HistoryEntry struct {
	Message     HistoryMessage `json:"message"`
	ExecutionID string         `json:"executionId,omitempty"`
	PromptLogs  []PromptLog    `json:"promptLogs,omitempty"`
}

// HistoryMessage is the role+content of a single message.
type HistoryMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
	ID      string          `json:"id"`
}

// PromptLog records an LLM call within a history entry.
type PromptLog struct {
	ModelTitle string `json:"modelTitle"`
	Prompt     string `json:"prompt"`
}

// ExecutionIndex is the top-level execution metadata file.
type ExecutionIndex struct {
	Executions []ExecutionMeta `json:"executions"`
	Version    string          `json:"version"`
}

// ExecutionMeta is a single execution entry in the index.
type ExecutionMeta struct {
	ExecutionID string `json:"executionId"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	StartTime   int64  `json:"startTime"`
	EndTime     int64  `json:"endTime"`
}

// ExecutionTrace is a full execution trace file.
type ExecutionTrace struct {
	ExecutionID   string            `json:"executionId"`
	WorkflowType  string            `json:"workflowType"`
	Status        string            `json:"status"`
	StartTime     int64             `json:"startTime"`
	ChatSessionID string            `json:"chatSessionId"`
	Actions       []ExecutionAction `json:"actions"`
}

// ExecutionAction is a single action within an execution trace.
type ExecutionAction struct {
	Type          string          `json:"type"`
	ExecutionID   string          `json:"executionId"`
	ActionID      string          `json:"actionId"`
	ActionType    string          `json:"actionType"`
	ActionState   string          `json:"actionState"`
	Input         json.RawMessage `json:"input,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	ChatSessionID string          `json:"chatSessionId"`
	EmittedAt     int64           `json:"emittedAt"`
}

// CreateInput is the parsed input for a "create" action.
type CreateInput struct {
	File            string `json:"file"`
	ModifiedContent string `json:"modifiedContent"`
	OriginalContent string `json:"originalContent"`
}

// AppendInput is the parsed input for an "append" action.
type AppendInput struct {
	File            string `json:"file"`
	ModifiedContent string `json:"modifiedContent"`
	OriginalContent string `json:"originalContent"`
}

// RelocateInput is the parsed input for a "smartRelocate" action.
type RelocateInput struct {
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
}
