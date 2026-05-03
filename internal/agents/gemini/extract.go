package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// toolUse matches the structure used by other providers for interoperability.
type toolUse struct {
	Name     string `json:"name"`
	FilePath string `json:"file_path,omitempty"`
	FileOp   string `json:"file_op,omitempty"`
}

// geminiTranscript is the normalized Gemini transcript shape used by
// the hook layer.
type geminiTranscript struct {
	SessionID string          `json:"-"`
	Messages  []geminiMessage `json:"messages"`
}

// geminiMessage represents a single message in the transcript.
type geminiMessage struct {
	ID        string           `json:"id,omitempty"`
	Type      string           `json:"type"`
	Content   string           `json:"-"` // custom UnmarshalJSON
	ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
	Tokens    *geminiTokens    `json:"tokens,omitempty"`
	Model     string           `json:"model,omitempty"`
}

// UnmarshalJSON handles both string and array content formats.
// User messages use: "content": [{"text": "..."}]
// Gemini messages use: "content": "response text"
func (m *geminiMessage) UnmarshalJSON(data []byte) error {
	type Alias geminiMessage
	aux := &struct {
		*Alias
		Content json.RawMessage `json:"content,omitempty"`
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	if len(aux.Content) == 0 || string(aux.Content) == "null" {
		m.Content = ""
		return nil
	}

	// Try string first (most common for gemini messages).
	var strContent string
	if err := json.Unmarshal(aux.Content, &strContent); err == nil {
		m.Content = strContent
		return nil
	}

	// Try array of objects with "text" fields (user messages).
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(aux.Content, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		m.Content = strings.Join(texts, "\n")
		return nil
	}

	return nil
}

// geminiToolCall represents a tool call in a gemini message.
type geminiToolCall struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Status string         `json:"status,omitempty"`
}

// geminiTokens represents token usage from a Gemini API response.
type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// parseTranscript parses both legacy JSON transcripts and JSONL
// transcripts with a header record followed by message records.
func parseTranscript(data []byte) (*geminiTranscript, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return &geminiTranscript{}, nil
	}

	// Try legacy JSON first. The legacy shape has a "messages" key,
	// so a successful parse with messages means we are done.
	var legacy geminiTranscript
	if err := json.Unmarshal(data, &legacy); err == nil && len(legacy.Messages) > 0 {
		return &legacy, nil
	}

	// Fall through to JSONL.
	return parseTranscriptJSONL(trimmed)
}

// parseTranscriptJSONL reads Gemini JSONL transcripts. Header records
// populate SessionID, and message records populate Messages.
func parseTranscriptJSONL(data []byte) (*geminiTranscript, error) {
	var t geminiTranscript
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Allow message lines up to 16 MiB so long assistant responses
	// or tool-response payloads do not truncate silently.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var hdr struct {
			SessionID string `json:"sessionId"`
			Kind      string `json:"kind"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			// Skip lines that fail to parse rather than aborting the
			// whole file. Replay should be best-effort.
			continue
		}
		if hdr.SessionID != "" && hdr.Type == "" && hdr.Kind != "" {
			if t.SessionID == "" {
				t.SessionID = hdr.SessionID
			}
			continue
		}
		// Only message records carry a type field. Skip anything else
		// (future metadata records, trailing markers) so t.Messages
		// stays a strict message slice and matches the doc comment.
		if hdr.Type == "" {
			continue
		}
		var msg geminiMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		t.Messages = append(t.Messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
	}
	return &t, nil
}

// readSessionIDFromHeader returns the JSONL header sessionId from the
// first non-empty line, or "" for non-JSONL/invalid inputs.
func readSessionIDFromHeader(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var hdr struct {
			SessionID string `json:"sessionId"`
			Kind      string `json:"kind"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			return ""
		}
		if hdr.SessionID != "" && hdr.Type == "" && hdr.Kind != "" {
			return hdr.SessionID
		}
		return ""
	}
	return ""
}

// extractToolUses returns tool call metadata from a gemini message.
func extractToolUses(msg geminiMessage) []toolUse {
	var result []toolUse
	for _, tc := range msg.ToolCalls {
		name := tc.Name
		if name == "" {
			continue
		}
		fp := extractFilePathFromArgs(tc.Args)
		result = append(result, toolUse{
			Name:     name,
			FilePath: fp,
			FileOp:   mapGeminiToolOp(name),
		})
	}
	return result
}

// extractFilePathFromArgs tries to pull a file path from tool call args.
func extractFilePathFromArgs(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "filename", "file"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// mapGeminiToolOp maps Gemini tool names to generic file operation types.
func mapGeminiToolOp(toolName string) string {
	lower := strings.ToLower(toolName)
	switch {
	case lower == "write_file" || lower == "save_file" || strings.Contains(lower, "create"):
		return "write"
	case lower == "edit_file" || lower == "replace" || strings.Contains(lower, "edit"):
		return "edit"
	case strings.Contains(lower, "read") || strings.Contains(lower, "search") || strings.Contains(lower, "grep"):
		return "read"
	case strings.Contains(lower, "shell") || strings.Contains(lower, "exec") || strings.Contains(lower, "bash") || strings.Contains(lower, "command"):
		return "exec"
	default:
		return ""
	}
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
