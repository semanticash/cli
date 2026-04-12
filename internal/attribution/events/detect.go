package events

import (
	"encoding/json"
	"strings"
)

// HasEditOrWrite returns true if the tool_uses JSON contains an Edit or Write
// tool call. Used as a fast pre-filter before payload loading.
func HasEditOrWrite(toolUses string) bool {
	if toolUses == "" {
		return false
	}
	return strings.Contains(toolUses, `"Edit"`) || strings.Contains(toolUses, `"Write"`)
}

// HasProviderFileEdit returns true if the tool_uses JSON indicates a provider
// file edit event. Matches tool names from Cursor, Copilot, Kiro, and Gemini
// that represent file modifications without line-level payload content.
func HasProviderFileEdit(toolUses string) bool {
	if toolUses == "" {
		return false
	}
	return strings.Contains(toolUses, `"cursor_file_edit"`) ||
		strings.Contains(toolUses, `"cursor_edit"`) ||
		strings.Contains(toolUses, `"copilot_file_edit"`) ||
		strings.Contains(toolUses, `"kiro_file_edit"`) ||
		strings.Contains(toolUses, `"editFile"`) ||
		strings.Contains(toolUses, `"createFile"`) ||
		strings.Contains(toolUses, `"write_file"`) ||
		strings.Contains(toolUses, `"edit_file"`) ||
		strings.Contains(toolUses, `"save_file"`) ||
		strings.Contains(toolUses, `"replace"`)
}

// ExtractProviderFileTouches parses the tool_uses JSON and returns
// repo-relative file paths that the AI touched.
func ExtractProviderFileTouches(toolUses string) []string {
	if toolUses == "" {
		return nil
	}
	var payload struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUses), &payload); err != nil {
		return nil
	}
	var paths []string
	for _, t := range payload.Tools {
		if t.FilePath != "" {
			paths = append(paths, t.FilePath)
		}
	}
	return paths
}
