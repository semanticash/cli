package kiro

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileOperation represents a single file change extracted from an execution trace.
//
// ActionType preserves the trace's original value: "create", "replace"
// (current Kiro IDE), "append" (legacy, identical shape to replace), or
// "smartRelocate". OriginalContent is the pre-edit content for replace and
// append; empty for create (new file) and smartRelocate (rename, no diff).
// ActionID is Kiro's per-action identifier (e.g. "tooluse_<random>"); used
// downstream to keep two edits to the same file in the same execution
// distinct.
type FileOperation struct {
	ActionType      string
	ActionID        string
	FilePath        string // relative path within the workspace
	Content         string // the content the agent wrote (empty for relocate)
	OriginalContent string // pre-edit content for replace/append; empty otherwise
	SourcePath      string // for relocate: original path
	DestPath        string // for relocate: new path
	EmittedAt       int64  // unix ms timestamp of the action
}

// ExtractFileOps parses an execution trace and returns all file operations.
//
// The "replace" action type (current Kiro IDE) shares the AppendInput shape
// with the legacy "append" but is intentionally not handled here yet:
// downstream callers do not yet route unknown action types safely, so adding
// "replace" alongside this extractor change in isolation would surface
// phantom events with empty fields. Both action types will be handled
// together with the matching downstream wire-up.
func ExtractFileOps(trace *ExecutionTrace) []FileOperation {
	var ops []FileOperation
	for _, action := range trace.Actions {
		switch action.ActionType {
		case "create":
			var input CreateInput
			if json.Unmarshal(action.Input, &input) != nil || input.File == "" {
				continue
			}
			ops = append(ops, FileOperation{
				ActionType:      "create",
				ActionID:        action.ActionID,
				FilePath:        input.File,
				Content:         input.ModifiedContent,
				OriginalContent: input.OriginalContent,
				EmittedAt:       action.EmittedAt,
			})

		case "append":
			var input AppendInput
			if json.Unmarshal(action.Input, &input) != nil || input.File == "" {
				continue
			}
			ops = append(ops, FileOperation{
				ActionType:      "append",
				ActionID:        action.ActionID,
				FilePath:        input.File,
				Content:         input.ModifiedContent,
				OriginalContent: input.OriginalContent,
				EmittedAt:       action.EmittedAt,
			})

		case "smartRelocate":
			var input RelocateInput
			if json.Unmarshal(action.Input, &input) != nil {
				continue
			}
			ops = append(ops, FileOperation{
				ActionType: "smartRelocate",
				ActionID:   action.ActionID,
				SourcePath: input.SourcePath,
				DestPath:   input.DestinationPath,
				EmittedAt:  action.EmittedAt,
			})
		}
	}
	return ops
}

// ParseExecutionTrace reads and parses an execution trace JSON file.
func ParseExecutionTrace(path string) (*ExecutionTrace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read execution trace: %w", err)
	}
	var trace ExecutionTrace
	if err := json.Unmarshal(data, &trace); err != nil {
		return nil, fmt.Errorf("parse execution trace: %w", err)
	}
	return &trace, nil
}

// ParseSessionIndex reads sessions.json and returns all session entries.
func ParseSessionIndex(path string) ([]SessionIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sessions.json: %w", err)
	}
	var sessions []SessionIndex
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, fmt.Errorf("parse sessions.json: %w", err)
	}
	return sessions, nil
}

// ParseSessionHistory reads a session history file (<sessionId>.json).
func ParseSessionHistory(path string) (*SessionHistory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	var history SessionHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("parse session history: %w", err)
	}
	return &history, nil
}

// CountExecutionEntries returns the number of history entries with an
// execution ID.
func CountExecutionEntries(h *SessionHistory) int {
	count := 0
	for _, entry := range h.History {
		if entry.ExecutionID != "" {
			count++
		}
	}
	return count
}

// NewExecutionIDs returns execution IDs beyond the given execution-entry
// offset.
func NewExecutionIDs(h *SessionHistory, offset int) []string {
	seen := 0
	var ids []string
	for _, entry := range h.History {
		if entry.ExecutionID == "" {
			continue
		}
		seen++
		if seen > offset {
			ids = append(ids, entry.ExecutionID)
		}
	}
	return ids
}

// KiroGlobalStorageDir returns the Kiro agent globalStorage directory for the
// current OS.
func KiroGlobalStorageDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// macOS
	dir := filepath.Join(home, "Library", "Application Support", "Kiro", "User", "globalStorage", "kiro.kiroagent")
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	// Windows
	if appData := os.Getenv("APPDATA"); appData != "" {
		dir = filepath.Join(appData, "Kiro", "User", "globalStorage", "kiro.kiroagent")
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}
	// Linux
	dir = filepath.Join(home, ".config", "Kiro", "User", "globalStorage", "kiro.kiroagent")
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	return "", fmt.Errorf("kiro globalStorage not found")
}

// EncodeWorkspacePath encodes an absolute workspace path to match the
// workspace session directory naming used by Kiro.
func EncodeWorkspacePath(absPath string) string {
	encoded := base64.URLEncoding.EncodeToString([]byte(absPath))
	return strings.ReplaceAll(encoded, "=", "_")
}

// decodeWorkspacePath reverses EncodeWorkspacePath.
func decodeWorkspacePath(encoded string) string {
	padded := strings.ReplaceAll(encoded, "_", "=")
	decoded, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// WorkspaceSessionDir returns the path to the Kiro workspace session
// directory for the given workspace path.
func WorkspaceSessionDir(absWorkspacePath string) (string, error) {
	globalDir, err := KiroGlobalStorageDir()
	if err != nil {
		return "", err
	}
	encoded := EncodeWorkspacePath(absWorkspacePath)
	dir := filepath.Join(globalDir, "workspace-sessions", encoded)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("workspace session dir not found: %s", dir)
	}
	return dir, nil
}

// ResolveLatestSession finds the most recent session for a workspace.
// Returns the session ID and path to the session history JSON file.
func ResolveLatestSession(workspacePath string) (sessionID, historyPath string, err error) {
	sessDir, err := WorkspaceSessionDir(workspacePath)
	if err != nil {
		return "", "", err
	}
	return ResolveLatestSessionIn(sessDir)
}

// ResolveLatestSessionIn finds the most recent session within a workspace
// session directory.
func ResolveLatestSessionIn(sessDir string) (sessionID, historyPath string, err error) {
	sessions, err := ParseSessionIndex(filepath.Join(sessDir, "sessions.json"))
	if err != nil {
		return "", "", err
	}
	if len(sessions) == 0 {
		return "", "", fmt.Errorf("no sessions found in %s", sessDir)
	}

	// sessions.json is ordered by creation time, oldest to newest.
	latest := sessions[len(sessions)-1]
	histPath := filepath.Join(sessDir, latest.SessionID+".json")
	if _, err := os.Stat(histPath); err != nil {
		return "", "", fmt.Errorf("session history not found: %s", histPath)
	}

	return latest.SessionID, histPath, nil
}
