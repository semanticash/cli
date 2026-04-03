package cursor

import (
	"encoding/json"
	"strings"
)

// toolUse matches the structure used by the Claude provider for interoperability.
type toolUse struct {
	Name     string `json:"name"`
	FilePath string `json:"file_path,omitempty"`
	FileOp   string `json:"file_op,omitempty"`
}

// composerData holds parsed fields from a composerData:<id> KV entry.
type composerData struct {
	ComposerID    string
	Title         string
	Model         string
	CreatedAt     int64 // unix milliseconds
	LastUpdatedAt int64 // unix milliseconds
}

// bubbleData holds parsed fields from a bubbleId:<composerId>:<bubbleId> KV entry.
type bubbleData struct {
	Role         string    // "user" or "assistant"
	Kind         string    // event type: "user", "assistant", or "tool_result"
	ContentTypes []string  // e.g. ["text", "tool_use"] - matches Claude provider format
	Summary      string    // truncated text content
	ToolUses     []toolUse // extracted tool calls
	Timestamp    int64     // unix milliseconds (best-effort)
}

// parseComposerData extracts session metadata from a composerData JSON value.
func parseComposerData(value string) composerData {
	var raw struct {
		ComposerID    string  `json:"composerId"`
		Title         string  `json:"title"`
		Model         string  `json:"model"`
		CreatedAt     float64 `json:"createdAt"`
		LastUpdatedAt float64 `json:"lastUpdatedAt"`
	}
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return composerData{}
	}
	return composerData{
		ComposerID:    raw.ComposerID,
		Title:         raw.Title,
		Model:         raw.Model,
		CreatedAt:     normalizeTimestamp(raw.CreatedAt),
		LastUpdatedAt: normalizeTimestamp(raw.LastUpdatedAt),
	}
}

// parseBubble extracts message fields from a bubble JSON value.
func parseBubble(value string) bubbleData {
	var raw struct {
		Type           float64         `json:"type"`
		Text           string          `json:"text"`
		ToolFormerData json.RawMessage `json:"toolFormerData"`
		CreatedAt      float64         `json:"createdAt"`
	}
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return bubbleData{}
	}

	bd := bubbleData{
		Timestamp: normalizeTimestamp(raw.CreatedAt),
	}

	switch int(raw.Type) {
	case 1:
		bd.Role = "user"
		bd.Kind = "user"
	case 2:
		bd.Role = "assistant"
		bd.Kind = "assistant"
	default:
		// Unknown type - skip.
		return bubbleData{}
	}

	if raw.Text != "" {
		bd.Summary = truncate(raw.Text, 200)
		bd.ContentTypes = append(bd.ContentTypes, "text")
	}

	if raw.ToolFormerData != nil {
		bd.ToolUses = extractCursorToolUses(raw.ToolFormerData)
		if len(bd.ToolUses) > 0 {
			bd.ContentTypes = append(bd.ContentTypes, "tool_use")
		}
	}

	return bd
}

// extractCursorToolUses parses tool data from Cursor's toolFormerData field.
//
// The format varies across Cursor versions:
//   - Array of tool objects: [{"name":"edit_file","path":"/foo.go"}, ...]
//   - Single tool object:   {"name":"edit_file","toolCallId":"...","rawArgs":"..."}
//   - Object with nested array: {"tools":[...]} or {"calls":[...]}
//
// We try each shape and extract what we can.
func extractCursorToolUses(data json.RawMessage) []toolUse {
	// Try 1: array of objects (most common).
	var arr []json.RawMessage
	if json.Unmarshal(data, &arr) == nil && len(arr) > 0 {
		var result []toolUse
		for _, raw := range arr {
			if tu := parseOneToolCall(raw); tu != nil {
				result = append(result, *tu)
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Try 2: single tool object.
	if tu := parseOneToolCall(data); tu != nil {
		return []toolUse{*tu}
	}

	// Try 3: wrapper object with a nested array field.
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(data, &wrapper) == nil {
		for _, key := range []string{"tools", "calls", "toolCalls", "tool_calls"} {
			if nested, ok := wrapper[key]; ok {
				if result := extractCursorToolUses(nested); len(result) > 0 {
					return result
				}
			}
		}
		// Scan all values for arrays that might contain tool objects.
		for _, v := range wrapper {
			var items []json.RawMessage
			if json.Unmarshal(v, &items) == nil && len(items) > 0 {
				var result []toolUse
				for _, item := range items {
					if tu := parseOneToolCall(item); tu != nil {
						result = append(result, *tu)
					}
				}
				if len(result) > 0 {
					return result
				}
			}
		}
	}

	return nil
}

// parseOneToolCall tries to extract a single toolUse from a JSON object.
// Handles both direct fields and Cursor's rawArgs stringified-JSON pattern.
func parseOneToolCall(data json.RawMessage) *toolUse {
	var obj struct {
		Name     string          `json:"name"`
		Type     string          `json:"type"`
		Tool     json.RawMessage `json:"tool"`      // sometimes numeric tool id
		FilePath string          `json:"file_path"`
		Path     string          `json:"path"`
		Input    json.RawMessage `json:"input"`
		RawArgs  string          `json:"rawArgs"` // stringified JSON with args
	}
	if json.Unmarshal(data, &obj) != nil {
		return nil
	}

	name := obj.Name
	if name == "" {
		name = obj.Type
	}
	// "tool" field can be a string name.
	if name == "" && obj.Tool != nil {
		var toolStr string
		if json.Unmarshal(obj.Tool, &toolStr) == nil && toolStr != "" {
			name = toolStr
		}
	}
	if name == "" {
		return nil
	}

	fp := obj.FilePath
	if fp == "" {
		fp = obj.Path
	}
	// Try input object for file_path.
	if fp == "" && obj.Input != nil {
		fp = extractFilePath(obj.Input)
	}
	// Try rawArgs (stringified JSON) for file_path.
	if fp == "" && obj.RawArgs != "" {
		fp = extractFilePath(json.RawMessage(obj.RawArgs))
	}

	return &toolUse{
		Name:     name,
		FilePath: fp,
		FileOp:   mapCursorToolOp(name),
	}
}

// extractFilePath tries to pull a file path from a JSON object.
func extractFilePath(data json.RawMessage) string {
	var obj struct {
		FilePath string `json:"file_path"`
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

// mapCursorToolOp maps Cursor tool names to generic file operation types.
func mapCursorToolOp(toolName string) string {
	lower := strings.ToLower(toolName)
	switch {
	case strings.Contains(lower, "write") || strings.Contains(lower, "create"):
		return "write"
	case strings.Contains(lower, "edit") || strings.Contains(lower, "replace") || strings.Contains(lower, "apply"):
		return "edit"
	case strings.Contains(lower, "read") || strings.Contains(lower, "search") || strings.Contains(lower, "grep") || strings.Contains(lower, "glob"):
		return "read"
	case strings.Contains(lower, "terminal") || strings.Contains(lower, "command") || strings.Contains(lower, "exec") || strings.Contains(lower, "bash") || strings.Contains(lower, "shell"):
		return "exec"
	default:
		return ""
	}
}

// normalizeTimestamp converts a numeric timestamp to unix milliseconds.
// Handles seconds, milliseconds, and nanoseconds.
func normalizeTimestamp(f float64) int64 {
	if f == 0 {
		return 0
	}
	if f > 1e15 {
		return int64(f / 1e6) // nanoseconds -> ms
	}
	if f > 1e12 {
		return int64(f) // already ms
	}
	return int64(f * 1000) // seconds -> ms
}


// parseCursorJSONLLine extracts role, kind, summary, tool uses, and content
// types from a Cursor agent-transcripts JSONL line. The format mirrors Claude Code:
//
//	{"role":"user","message":{"content":[{"type":"text","text":"..."},{"type":"tool_use","name":"Shell","input":{...}}]}}
func parseCursorJSONLLine(line string) bubbleData {
	var raw struct {
		Role    string `json:"role"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return bubbleData{}
	}

	bd := bubbleData{
		Role: raw.Role,
		Kind: raw.Role, // "user" or "assistant"
	}
	if bd.Role == "" {
		return bubbleData{}
	}

	hasText := false
	hasToolUse := false
	for _, c := range raw.Message.Content {
		switch c.Type {
		case "text":
			if c.Text != "" && !hasText {
				text := c.Text
				if bd.Role == "user" {
					text = stripUserQueryTags(text)
				}
				bd.Summary = truncate(text, 200)
				hasText = true
			}
		case "tool_use":
			if c.Name != "" {
				tu := toolUse{
					Name:   c.Name,
					FileOp: mapCursorToolOp(c.Name),
				}
				if c.Input != nil {
					tu.FilePath = extractJSONLToolPath(c.Input)
				}
				bd.ToolUses = append(bd.ToolUses, tu)
				hasToolUse = true
			}
		}
	}

	if hasText {
		bd.ContentTypes = append(bd.ContentTypes, "text")
	}
	if hasToolUse {
		bd.ContentTypes = append(bd.ContentTypes, "tool_use")
	}

	return bd
}

// extractJSONLToolPath extracts a file path from a Cursor JSONL tool_use input.
// Handles common Cursor tool input shapes:
//   - {"file_path": "..."} - file-oriented tools
//   - {"path": "..."} - some tools
//   - {"working_directory": "..."} - Shell tool
//   - {"target_directory": "..."} - Glob tool
func extractJSONLToolPath(input json.RawMessage) string {
	var obj struct {
		FilePath         string `json:"file_path"`
		Path             string `json:"path"`
		File             string `json:"file"`
		WorkingDirectory string `json:"working_directory"`
		TargetDirectory  string `json:"target_directory"`
	}
	if json.Unmarshal(input, &obj) != nil {
		return ""
	}
	for _, p := range []string{obj.FilePath, obj.Path, obj.File, obj.WorkingDirectory, obj.TargetDirectory} {
		if p != "" {
			return p
		}
	}
	return ""
}

// stripUserQueryTags removes <user_query>...</user_query> wrapper tags
// from Cursor user messages.
func stripUserQueryTags(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<user_query>")
	s = strings.TrimSuffix(s, "</user_query>")
	return strings.TrimSpace(s)
}

// truncate trims and limits a string to max characters.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max]
	}
	return s
}
