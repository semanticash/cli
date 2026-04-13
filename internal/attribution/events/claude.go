package events

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
)

// ExtractClaudeActions parses a Claude-format assistant payload and extracts:
// - fileLines: map of repo-relative file paths to sets of trimmed lines (from Edit/Write tools)
// - bashCommands: shell commands from Bash tool calls
func ExtractClaudeActions(raw []byte, repoRoot string) (fileLines map[string]map[string]struct{}, bashCommands []string) {
	fileLines = make(map[string]map[string]struct{})

	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if payload.Type != "assistant" {
		return
	}

	for _, blockRaw := range payload.Message.Content {
		var block struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		if block.Type != "tool_use" {
			continue
		}

		switch block.Name {
		case "Edit":
			var inp struct {
				FilePath  string `json:"file_path"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.NewString == "" {
				continue
			}
			relPath := NormalizePath(inp.FilePath, repoRoot)
			AddLines(fileLines, relPath, inp.NewString)

		case "Write":
			var inp struct {
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.Content == "" {
				continue
			}
			relPath := NormalizePath(inp.FilePath, repoRoot)
			AddLines(fileLines, relPath, inp.Content)

		case "Bash":
			var inp struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.Command == "" {
				continue
			}
			bashCommands = append(bashCommands, inp.Command)
		}
	}

	return
}

// ExtractDeletedPaths extracts file paths from a shell command containing "rm".
// Returns repo-relative paths.
func ExtractDeletedPaths(cmd, repoRoot string) []string {
	if !strings.Contains(cmd, "rm ") {
		return nil
	}

	var paths []string
	for _, token := range strings.Fields(cmd) {
		if token == "rm" || strings.HasPrefix(token, "-") {
			continue
		}
		rel := NormalizePath(token, repoRoot)
		if rel != "" && rel != "." {
			paths = append(paths, rel)
		}
	}
	return paths
}

// NormalizePath converts an absolute file path to a repo-relative path
// using forward slashes, matching the format produced by "git diff".
// Handles MSYS-style paths (/c/Users/...) from Claude Code on Windows.
func NormalizePath(filePath, repoRoot string) string {
	if filePath == "" {
		return ""
	}
	// Convert MSYS paths (/c/Users/...) to Windows paths (C:/Users/...).
	if runtime.GOOS == "windows" && len(filePath) >= 3 && filePath[0] == '/' && filePath[2] == '/' {
		filePath = strings.ToUpper(string(filePath[1])) + ":" + filePath[2:]
	}
	// Clean both paths so mixed separators (forward vs back) are normalized
	// before filepath.Rel computes the relative path.
	rel, err := filepath.Rel(filepath.Clean(repoRoot), filepath.Clean(filePath))
	if err != nil {
		return filepath.Base(filePath)
	}
	return filepath.ToSlash(rel)
}

// AddLines splits text into lines and inserts each trimmed, non-blank line
// into the set for the given file path.
func AddLines(m map[string]map[string]struct{}, filePath, text string) {
	if filePath == "" {
		return
	}
	if m[filePath] == nil {
		m[filePath] = make(map[string]struct{})
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		m[filePath][trimmed] = struct{}{}
	}
}
