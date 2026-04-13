package kirocli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"

	_ "modernc.org/sqlite"
)

// kiroCLIDBPath returns the path to the Kiro CLI SQLite database.
func kiroCLIDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// macOS
	p := filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	// Windows
	if appData := os.Getenv("APPDATA"); appData != "" {
		p = filepath.Join(appData, "kiro-cli", "data.sqlite3")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// Linux
	for _, dir := range []string{".local/share/kiro-cli", ".config/kiro-cli"} {
		p = filepath.Join(home, dir, "data.sqlite3")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("kiro-cli database not found")
}

// resolveLatestConversation returns the most recent conversation for a
// workspace from the Kiro CLI database.
func resolveLatestConversation(workspacePath string) (dbPath, conversationID string, err error) {
	dbPath, err = kiroCLIDBPath()
	if err != nil {
		return "", "", err
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return "", "", fmt.Errorf("open kiro-cli db: %w", err)
	}
	defer func() { _ = db.Close() }()

	var convID string
	err = db.QueryRow(
		`SELECT conversation_id FROM conversations_v2 WHERE key = ? ORDER BY updated_at DESC LIMIT 1`,
		workspacePath,
	).Scan(&convID)
	if err != nil {
		return "", "", fmt.Errorf("no kiro-cli conversation for workspace %s: %w", workspacePath, err)
	}

	return dbPath, convID, nil
}

// buildTranscriptRef creates a composite transcript reference.
func buildTranscriptRef(dbPath, conversationID string) string {
	return dbPath + "#" + conversationID
}

// parseTranscriptRef splits a composite transcript reference.
func parseTranscriptRef(ref string) (dbPath, conversationID string, err error) {
	idx := strings.LastIndex(ref, "#")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid transcript ref: %s", ref)
	}
	return ref[:idx], ref[idx+1:], nil
}

// loadConversation reads and parses the conversation JSON from the database.
func loadConversation(dbPath, conversationID string) (*conversationValue, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open kiro-cli db: %w", err)
	}
	defer func() { _ = db.Close() }()

	var valueJSON string
	err = db.QueryRow(
		`SELECT value FROM conversations_v2 WHERE conversation_id = ?`,
		conversationID,
	).Scan(&valueJSON)
	if err != nil {
		return nil, fmt.Errorf("conversation %s not found: %w", conversationID, err)
	}

	var conv conversationValue
	if err := json.Unmarshal([]byte(valueJSON), &conv); err != nil {
		return nil, fmt.Errorf("parse conversation: %w", err)
	}
	return &conv, nil
}

// extractToolCalls returns fs_write tool calls in conversation order.
func extractToolCalls(conv *conversationValue) []toolCallInfo {
	var calls []toolCallInfo
	for _, entry := range conv.History {
		var asst assistantToolUse
		if json.Unmarshal(entry.Assistant, &asst) != nil || asst.ToolUse == nil {
			continue
		}
		for _, tu := range asst.ToolUse.ToolUses {
			if tu.Name != "fs_write" {
				continue
			}
			var args fsWriteArgs
			if json.Unmarshal(tu.Args, &args) != nil {
				continue
			}
			calls = append(calls, toolCallInfo{
				ID:       tu.ID,
				FilePath: args.Path,
				FileText: args.FileText,
				Command:  args.Command,
			})
		}
	}
	return calls
}

type toolCallInfo struct {
	ID       string
	FilePath string
	FileText string
	Command  string // "create" or "edit"
}

// newToolCallsSince returns tool calls after lastSeenID. If lastSeenID is
// empty or no longer present, it returns all calls.
func newToolCallsSince(calls []toolCallInfo, lastSeenID string) []toolCallInfo {
	if lastSeenID == "" {
		return calls
	}
	found := false
	var result []toolCallInfo
	for _, c := range calls {
		if found {
			result = append(result, c)
		}
		if c.ID == lastSeenID {
			found = true
		}
	}
	if !found {
		return calls
	}
	return result
}

// toolCallsToEvents converts tool calls into broker RawEvents.
func toolCallsToEvents(calls []toolCallInfo, transcriptRef, workspacePath, providerSessionID string, timestamp int64) []broker.RawEvent {
	var events []broker.RawEvent
	for _, tc := range calls {
		fileOp := tc.Command
		if fileOp == "" {
			fileOp = "create"
		}

		event := broker.RawEvent{
			EventID:           hashToolCallID(tc.ID),
			Provider:          agentKiro.ProviderNameCLI,
			SourceKey:         transcriptRef,
			Kind:              "assistant",
			Role:              "assistant",
			Timestamp:         timestamp,
			ToolUsesJSON:      agentKiro.BuildToolUsesJSON(tc.FilePath, fileOp).String,
			Summary:           fmt.Sprintf("%s %s", fileOp, filepath.Base(tc.FilePath)),
			ProviderSessionID: providerSessionID,
			SourceProjectPath: workspacePath,
			EventSource:       "transcript",
			ToolUseID:         tc.ID,
			ToolName:          agentKiro.ToolNameFileEdit,
		}

		if tc.FilePath != "" {
			event.FilePaths = []string{tc.FilePath}
		}

		events = append(events, event)
	}
	return events
}

func hashToolCallID(toolCallID string) string {
	// Tool call IDs are already unique. Truncate for consistency with other
	// providers.
	if len(toolCallID) > 32 {
		return toolCallID[:32]
	}
	return toolCallID
}

func sidecarPath(wsKey string) (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "capture", "kirocli-offset-"+wsKey+".json"), nil
}

func readSidecar(wsKey string) (string, error) {
	p, err := sidecarPath(wsKey)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var s sidecarOffset
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	return s.LastToolUseID, nil
}

func writeSidecar(wsKey, lastToolUseID string) error {
	p, err := sidecarPath(wsKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(sidecarOffset{LastToolUseID: lastToolUseID})
	return os.WriteFile(p, data, 0o644)
}
