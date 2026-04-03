package copilot

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Copilot JSONL event types.
const (
	eventTypeUserMessage      = "user.message"
	eventTypeAssistantMsg     = "assistant.message"
	eventTypeToolExecDone     = "tool.execution_complete"
	eventTypeModelChange      = "session.model_change"
	eventTypeSessionShutdown  = "session.shutdown"
)

// copilotEvent is a single line in events.jsonl.
type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	ParentID  string          `json:"parentId"`
}

// userMessageData is the data payload for user.message events.
type userMessageData struct {
	Content string `json:"content"`
}

// assistantMessageData is the data payload for assistant.message events.
type assistantMessageData struct {
	Content      string             `json:"content"`
	OutputTokens int64              `json:"outputTokens"`
	InputTokens  int64              `json:"inputTokens"`
	ToolRequests []assistantToolUse `json:"toolRequests"`
}

type assistantToolUse struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
	Tool       toolRequest     `json:"tool"`
}

type toolRequest struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// toolExecCompleteData is the data payload for tool.execution_complete events.
type toolExecCompleteData struct {
	ToolCallID    string        `json:"toolCallId"`
	Model         string        `json:"model"`
	ToolTelemetry toolTelemetry `json:"toolTelemetry"`
}

// modelChangeData is the data payload for session.model_change events.
type modelChangeData struct {
	NewModel string `json:"newModel"`
}

// sessionShutdownData is the data payload for session.shutdown events.
type sessionShutdownData struct {
	ModelMetrics map[string]modelMetricData `json:"modelMetrics"`
}

type modelMetricData struct {
	Requests struct {
		Count int `json:"count"`
	} `json:"requests"`
	Usage struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"usage"`
}

type toolTelemetry struct {
	Properties toolProperties `json:"properties"`
}

// toolProperties contains string-encoded metadata from tool execution.
// filePaths is a JSON-encoded string array, e.g. "[\"path/to/file.txt\"]".
type toolProperties struct {
	FilePaths string `json:"filePaths"`
}

// parseLine extracts structured fields from a single Copilot JSONL line.
func parseLine(line string) parsedLine {
	var ev copilotEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return parsedLine{}
	}

	switch ev.Type {
	case eventTypeUserMessage:
		return parseUserMessage(ev.Data)
	case eventTypeAssistantMsg:
		return parseAssistantMessage(ev.Data)
	case eventTypeToolExecDone:
		return parseToolExecComplete(ev.Data)
	case eventTypeModelChange, eventTypeSessionShutdown:
		// Parsed by dedicated extractors, not the per-line pipeline.
		return parsedLine{}
	default:
		return parsedLine{}
	}
}

func parseUserMessage(data json.RawMessage) parsedLine {
	var d userMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return parsedLine{}
	}
	pl := parsedLine{
		Role:    "user",
		Kind:    "user",
		Summary: truncate(d.Content, 200),
	}
	if d.Content != "" {
		pl.ContentTypes = append(pl.ContentTypes, "text")
	}
	return pl
}

func parseAssistantMessage(data json.RawMessage) parsedLine {
	var d assistantMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return parsedLine{}
	}
	pl := parsedLine{
		Role:      "assistant",
		Kind:      "assistant",
		Summary:   truncate(d.Content, 200),
		TokensIn:  d.InputTokens,
		TokensOut: d.OutputTokens,
	}
	if d.Content != "" {
		pl.ContentTypes = append(pl.ContentTypes, "text")
	}

	for _, tr := range d.ToolRequests {
		name := tr.Name
		input := tr.Arguments
		if name == "" && tr.Tool.Name != "" {
			name = tr.Tool.Name
			input = tr.Tool.Input
		}
		if shouldSkipToolUse(name) {
			continue
		}

		tu := toolUse{
			Name:      name,
			FileOp:    mapToolOp(name),
			ToolUseID: tr.ToolCallID,
		}
		if input != nil {
			tu.FilePath = extractInputFilePath(input)
		}
		pl.ToolUses = append(pl.ToolUses, tu)
	}
	if len(pl.ToolUses) > 0 {
		pl.ContentTypes = append(pl.ContentTypes, "tool_use")
	}

	return pl
}

func parseToolExecComplete(data json.RawMessage) parsedLine {
	var d toolExecCompleteData
	if err := json.Unmarshal(data, &d); err != nil {
		return parsedLine{}
	}

	if d.ToolTelemetry.Properties.FilePaths == "" {
		return parsedLine{Role: "tool", Kind: "tool_result"}
	}

	// Double deserialization: filePaths is a JSON-encoded string array.
	var paths []string
	if err := json.Unmarshal([]byte(d.ToolTelemetry.Properties.FilePaths), &paths); err != nil {
		return parsedLine{Role: "tool", Kind: "tool_result"}
	}

	var filePaths []string
	var tus []toolUse
	seen := make(map[string]bool)
	for _, fp := range paths {
		if fp == "" {
			continue
		}
		cleaned := filepath.Clean(fp)
		if !seen[cleaned] {
			seen[cleaned] = true
			filePaths = append(filePaths, cleaned)
			tus = append(tus, toolUse{
				Name:      "copilot_file_edit",
				FilePath:  cleaned,
				FileOp:    "edit",
				ToolUseID: d.ToolCallID,
			})
		}
	}

	pl := parsedLine{
		Role:      "tool",
		Kind:      "tool_result",
		FilePaths: filePaths,
		ToolUses:  tus,
	}
	if len(tus) > 0 {
		pl.ContentTypes = append(pl.ContentTypes, "tool_use")
	}
	return pl
}

// extractInputFilePath tries to pull a file path from a tool request's input JSON.
func extractInputFilePath(data json.RawMessage) string {
	var obj struct {
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
		File     string `json:"file"`
	}
	if json.Unmarshal(data, &obj) != nil {
		return ""
	}
	if obj.FilePath != "" {
		return obj.FilePath
	}
	if obj.Path != "" {
		return obj.Path
	}
	return obj.File
}

// mapToolOp maps Copilot tool names to generic file operation types.
func mapToolOp(toolName string) string {
	lower := strings.ToLower(toolName)
	switch {
	case lower == "edit":
		return "edit"
	case strings.Contains(lower, "editfile") || strings.Contains(lower, "edit_file"):
		return "edit"
	case lower == "create":
		return "create"
	case strings.Contains(lower, "createfile") || strings.Contains(lower, "create_file"):
		return "create"
	case lower == "read":
		return "read"
	case strings.Contains(lower, "readfile") || strings.Contains(lower, "read_file"):
		return "read"
	case strings.Contains(lower, "runcommand") || strings.Contains(lower, "run_command") ||
		strings.Contains(lower, "bash") || strings.Contains(lower, "shell"):
		return "exec"
	default:
		return ""
	}
}

func shouldSkipToolUse(toolName string) bool {
	return strings.EqualFold(strings.TrimSpace(toolName), "report_intent")
}

// extractModelFromLine extracts the LLM model name from a JSONL line.
// Returns non-empty for session.model_change and tool.execution_complete events.
func extractModelFromLine(line string) string {
	var ev copilotEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	switch ev.Type {
	case eventTypeModelChange:
		var d modelChangeData
		if json.Unmarshal(ev.Data, &d) == nil && d.NewModel != "" {
			return d.NewModel
		}
	case eventTypeToolExecDone:
		var d toolExecCompleteData
		if json.Unmarshal(ev.Data, &d) == nil && d.Model != "" {
			return d.Model
		}
	}
	return ""
}

// SessionTokens holds session-level token aggregates from session.shutdown.
type SessionTokens struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	APICallCount     int
}

// extractSessionShutdownTokens extracts aggregate token usage from a
// session.shutdown JSONL line. Returns nil if the line is not a shutdown event.
func extractSessionShutdownTokens(line string) *SessionTokens {
	var ev copilotEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type != eventTypeSessionShutdown {
		return nil
	}
	var d sessionShutdownData
	if err := json.Unmarshal(ev.Data, &d); err != nil || len(d.ModelMetrics) == 0 {
		return nil
	}
	var st SessionTokens
	for _, m := range d.ModelMetrics {
		st.InputTokens += m.Usage.InputTokens
		st.OutputTokens += m.Usage.OutputTokens
		st.CacheReadTokens += m.Usage.CacheReadTokens
		st.CacheWriteTokens += m.Usage.CacheWriteTokens
		st.APICallCount += m.Requests.Count
	}
	return &st
}
