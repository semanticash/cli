package events

import (
	"encoding/json"
	"strings"

	"github.com/semanticash/cli/internal/platform"
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
// file edit event. Matches tool names from providers that report file
// modifications without an accompanying line-level payload: Cursor, Copilot,
// Kiro, Gemini, and Codex apply_patch operations whose envelope produced no
// new content (deletions, empty-file adds, the source half of a rename).
func HasProviderFileEdit(toolUses string) bool {
	if toolUses == "" {
		return false
	}
	return strings.Contains(toolUses, `"cursor_file_edit"`) ||
		strings.Contains(toolUses, `"cursor_edit"`) ||
		strings.Contains(toolUses, `"copilot_file_edit"`) ||
		strings.Contains(toolUses, `"kiro_file_edit"`) ||
		strings.Contains(toolUses, `"codex_file_edit"`) ||
		strings.Contains(toolUses, `"editFile"`) ||
		strings.Contains(toolUses, `"createFile"`) ||
		strings.Contains(toolUses, `"write_file"`) ||
		strings.Contains(toolUses, `"edit_file"`) ||
		strings.Contains(toolUses, `"save_file"`) ||
		strings.Contains(toolUses, `"replace"`)
}

// ExtractProviderFileTouches parses the tool_uses JSON and returns
// repo-relative file paths that the AI touched. Paths that look
// absolute are relativized against repoRoot via NormalizePath so the
// scorer's diff-keyed lookup (which uses git's repo-relative paths)
// matches regardless of whether the hook layer stored an absolute
// path. Paths that already look relative pass through unchanged so
// providers that emit subdir-relative paths in their hook payloads
// (and whose hook payload `cwd` matched the repo root at emit time)
// keep their existing behavior.
func ExtractProviderFileTouches(toolUses, repoRoot string) []string {
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
		if t.FilePath == "" {
			continue
		}
		fp := t.FilePath
		if repoRoot != "" && platform.LooksAbsolutePath(fp) {
			fp = NormalizePath(fp, repoRoot)
		}
		paths = append(paths, fp)
	}
	return paths
}
