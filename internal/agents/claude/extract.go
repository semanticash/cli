package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type toolUse struct {
	Name      string `json:"name"`
	FilePath  string `json:"file_path,omitempty"`
	FileOp    string `json:"file_op,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

type extractedFields struct {
	Skip              bool
	Ts                int64
	Kind              string
	Role              string
	ContentTypes      []string  // e.g. ["thinking", "text", "tool_use"]
	ToolUses          []toolUse // all tool_use blocks
	TokensIn          int64     // base input_tokens (uncached)
	TokensOut         int64
	TokensCacheRead   int64 // cache_read_input_tokens
	TokensCacheCreate int64 // cache_creation_input_tokens
	Summary           string
	IsToolResult      bool // true when user-role event is a tool_result, not an actual user prompt
	IsFileReadResult  bool // true when tool_result contains file content from a Read tool
	ProviderEventID   string
	SessionID         string
	Model             string // LLM model name from assistant messages (e.g. "claude-sonnet-4-20250514")
}

// extractFields parses a single Claude JSONL line and returns structured fields.
func extractFields(line string) extractedFields {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return extractedFields{Ts: time.Now().UnixMilli(), Kind: "unknown"}
	}

	typ := jsonString(raw["type"])

	// Skip synthetic/noise types
	switch typ {
	case "progress", "file-history-snapshot", "queue-operation":
		return extractedFields{Skip: true}
	}

	f := extractedFields{
		Ts:        parseTimestamp(raw["timestamp"]),
		Kind:      typ,
		SessionID: jsonString(raw["sessionId"]),
	}
	if f.Kind == "" {
		f.Kind = "unknown"
	}

	switch typ {
	case "user":
		f.Role = "user"
		f.ProviderEventID = jsonString(raw["uuid"])
		f.Summary, f.IsToolResult = extractUserSummary(raw["message"])
		if f.IsToolResult {
			f.Kind = "tool_result"
			if summary, ok := extractFileReadResultSummary(raw["toolUseResult"]); ok {
				f.Summary = summary
				f.IsFileReadResult = true
			}
		}
	case "assistant":
		f.Role = "assistant"
		extractAssistantFields(raw["message"], &f)
	case "system":
		f.Role = "system"
	}

	return f
}

func extractAssistantFields(msgRaw json.RawMessage, f *extractedFields) {
	if msgRaw == nil {
		return
	}
	var msg struct {
		ID      string            `json:"id"`
		Model   string            `json:"model"`
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
		Usage   struct {
			InputTokens              float64 `json:"input_tokens"`
			CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
			OutputTokens             float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return
	}

	// Skip synthetic messages
	if msg.Model == "<synthetic>" {
		f.Skip = true
		return
	}

	f.ProviderEventID = msg.ID
	f.Model = msg.Model
	f.TokensIn = int64(msg.Usage.InputTokens)
	f.TokensOut = int64(msg.Usage.OutputTokens)
	f.TokensCacheRead = int64(msg.Usage.CacheReadInputTokens)
	f.TokensCacheCreate = int64(msg.Usage.CacheCreationInputTokens)

	// Track what content types are present to determine the event kind.
	hasThinking := false
	hasText := false
	hasToolUse := false

	// Walk content blocks, collect tool_use entries, thinking, and text.
	for _, blockRaw := range msg.Content {
		var block struct {
			Type     string          `json:"type"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "thinking":
			hasThinking = true
			if f.Summary == "" && block.Thinking != "" {
				f.Summary = truncate(block.Thinking, 200)
			}

		case "text":
			hasText = true
			if f.Summary == "" && block.Text != "" {
				f.Summary = truncate(block.Text, 200)
			}

		case "tool_use":
			hasToolUse = true
			tu := toolUse{
				Name:      block.Name,
				FileOp:    deriveFileOp(block.Name),
				ToolUseID: block.ID,
			}
			if block.Input != nil {
				var inp struct {
					FilePath string `json:"file_path"`
				}
				_ = json.Unmarshal(block.Input, &inp)
				tu.FilePath = inp.FilePath
			}
			f.ToolUses = append(f.ToolUses, tu)
		}
	}

	// Populate content types for provider-agnostic classification.
	if hasThinking {
		f.ContentTypes = append(f.ContentTypes, "thinking")
	}
	if hasText {
		f.ContentTypes = append(f.ContentTypes, "text")
	}
	if hasToolUse {
		f.ContentTypes = append(f.ContentTypes, "tool_use")
	}

	// Generate summary from tool uses if no text/thinking summary was found.
	if f.Summary == "" && len(f.ToolUses) > 0 {
		first := f.ToolUses[0]
		if first.FilePath != "" {
			f.Summary = truncate(first.Name+"("+first.FilePath+")", 200)
		} else {
			f.Summary = first.Name
		}
	}
}

// extractUserSummary parses a user-role message and returns the summary text
// along with whether the message is a tool_result (vs an actual user prompt).
//
// User messages come in two shapes:
//   - String content: the user typed a prompt
//   - Array content: tool_result blocks fed back to the model
//
// Tool result blocks can vary in shape (tool_use_id, is_error, nested content),
// so we unmarshal into []map[string]any for resilience.
func extractUserSummary(msgRaw json.RawMessage) (summary string, isToolResult bool) {
	if msgRaw == nil {
		return "", false
	}

	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return "", false
	}

	// Try string content first - this is an actual user prompt.
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return truncate(s, 200), false
	}

	// Array content - tool_result blocks or other structured content.
	// Use []map[string]any for resilience against varying block shapes
	// (tool_use_id, is_error, nested arrays, etc.).
	var blocks []map[string]any
	if err := json.Unmarshal(msg.Content, &blocks); err != nil || len(blocks) == 0 {
		return "", false
	}

	// Check whether any block is a tool_result.
	isToolResult = false
	for _, block := range blocks {
		if typ, _ := block["type"].(string); typ == "tool_result" {
			isToolResult = true
			break
		}
	}

	// Extract summary from the first block's content.
	first := blocks[0]
	switch content := first["content"].(type) {
	case string:
		return truncate(content, 200), isToolResult
	case []any:
		// Nested content array (e.g. tool_result with structured output).
		for _, item := range content {
			if m, ok := item.(map[string]any); ok {
				if text, _ := m["text"].(string); text != "" {
					return truncate(text, 200), isToolResult
				}
				if content, _ := m["content"].(string); content != "" {
					return truncate(content, 200), isToolResult
				}
			}
		}
	}

	return "", isToolResult
}

func extractFileReadResultSummary(raw json.RawMessage) (string, bool) {
	if raw == nil {
		return "", false
	}

	var result struct {
		Type string `json:"type"`
		File *struct {
			FilePath string `json:"filePath"`
			NumLines int    `json:"numLines"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false
	}
	if result.Type != "text" || result.File == nil || result.File.FilePath == "" {
		return "", false
	}

	summary := fmt.Sprintf("Read(%s)", result.File.FilePath)
	if result.File.NumLines > 0 {
		summary = fmt.Sprintf("%s -> %d lines", summary, result.File.NumLines)
	}
	return truncate(summary, 200), true
}

func hasOnlyReadToolUses(tus []toolUse) bool {
	if len(tus) == 0 {
		return false
	}
	for _, tu := range tus {
		if tu.Name != "Read" {
			return false
		}
	}
	return true
}

func deriveFileOp(toolName string) string {
	switch toolName {
	case "Write", "NotebookEdit":
		return "write"
	case "Edit":
		return "edit"
	case "Read", "Glob", "Grep":
		return "read"
	case "Bash":
		return "exec"
	default:
		return ""
	}
}

func parseTimestamp(raw json.RawMessage) int64 {
	if raw == nil {
		return time.Now().UnixMilli()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli()
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli()
		}
	}
	// Try numeric: >1e15 likely nanoseconds, >1e12 likely milliseconds, else seconds
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f > 1e15 {
			return int64(f / 1e6) // nanoseconds -> ms
		}
		if f > 1e12 {
			return int64(f) // already ms
		}
		return int64(f * 1000) // seconds -> ms
	}
	return time.Now().UnixMilli()
}

func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	// Replace newlines with spaces for summary
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max]
	}
	return s
}
