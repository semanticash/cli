package cursor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	agentcursor "github.com/semanticash/cli/internal/agents/cursor"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"

	_ "modernc.org/sqlite"
)

const providerName = "cursor"

// Provider implements hooks.HookProvider for Cursor IDE/CLI.
type Provider struct{}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "Cursor" }

func (p *Provider) IsAvailable() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); err == nil {
		return true
	}
	return false
}

const semanticaMarker = "semantica capture cursor"

type cursorHooksConfig struct {
	Version int                        `json:"version"`
	Hooks   map[string][]cursorHookDef `json:"hooks"`
}

type cursorHookDef struct {
	Command string `json:"command"`
}

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	hooksPath := filepath.Join(repoRoot, ".cursor", "hooks.json")

	var cfg cursorHooksConfig
	data, err := os.ReadFile(hooksPath)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return 0, fmt.Errorf("parse existing %s: %w", hooksPath, err)
		}
	}
	if cfg.Hooks == nil {
		cfg.Hooks = make(map[string][]cursorHookDef)
	}
	cfg.Version = 1

	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	hookDefs := []struct {
		hookPoint string
		command   string
	}{
		{"sessionStart", bin + " capture cursor session-start"},
		{"sessionEnd", bin + " capture cursor session-end"},
		{"beforeSubmitPrompt", bin + " capture cursor before-submit-prompt"},
		{"preToolUse", bin + " capture cursor pre-tool-use"},
		{"postToolUse", bin + " capture cursor post-tool-use"},
		{"afterFileEdit", bin + " capture cursor after-file-edit"},
		{"stop", bin + " capture cursor stop"},
		{"subagentStop", bin + " capture cursor subagent-stop"},
		{"preCompact", bin + " capture cursor pre-compact"},
	}

	count := 0
	for _, def := range hookDefs {
		existing := cfg.Hooks[def.hookPoint]
		found := false
		for _, h := range existing {
			if strings.Contains(h.Command, semanticaMarker) {
				found = true
				break
			}
		}
		if !found {
			cfg.Hooks[def.hookPoint] = append(existing, cursorHookDef{Command: def.command})
		}
		count++
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal hooks config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir .cursor: %w", err)
	}
	if err := os.WriteFile(hooksPath, out, 0o644); err != nil {
		return 0, fmt.Errorf("write hooks config: %w", err)
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	hooksPath := filepath.Join(repoRoot, ".cursor", "hooks.json")

	data, err := os.ReadFile(hooksPath)
	if err != nil {
		return nil
	}

	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	for hookPoint, defs := range cfg.Hooks {
		var kept []cursorHookDef
		for _, h := range defs {
			if !strings.Contains(h.Command, semanticaMarker) {
				kept = append(kept, h)
			}
		}
		if len(kept) > 0 {
			cfg.Hooks[hookPoint] = kept
		} else {
			delete(cfg.Hooks, hookPoint)
		}
	}

	out, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(hooksPath, out, 0o644)
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".cursor", "hooks.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), semanticaMarker)
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".cursor", "hooks.json"))
	if err != nil {
		return "", err
	}
	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	for _, defs := range cfg.Hooks {
		for _, h := range defs {
			if strings.Contains(h.Command, semanticaMarker) {
				parts := strings.Fields(h.Command)
				if len(parts) > 0 {
					return parts[0], nil
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hook found")
}

type stdinPayload struct {
	ConversationID     string          `json:"conversation_id"`
	GenerationID       string          `json:"generation_id,omitempty"`
	TranscriptPath     string          `json:"transcript_path,omitempty"`
	Prompt             string          `json:"prompt,omitempty"`
	Model              string          `json:"model,omitempty"`
	WorkspaceRoots     []string        `json:"workspace_roots,omitempty"`
	ToolName           string          `json:"tool_name,omitempty"`
	ToolUseID          string          `json:"tool_use_id,omitempty"`
	ToolInput          json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput         string          `json:"tool_output,omitempty"`
	CWD                string          `json:"cwd,omitempty"`
	FilePath           string          `json:"file_path,omitempty"`
	Edits              []cursorEdit    `json:"edits,omitempty"`
	SubagentID         string          `json:"subagent_id,omitempty"`
	SubagentType       string          `json:"subagent_type,omitempty"`
	AgentTranscriptRef string          `json:"agent_transcript_path,omitempty"`
	ParentConversation string          `json:"parent_conversation_id,omitempty"`
	Status             string          `json:"status,omitempty"`
	DurationMS         int64           `json:"duration_ms,omitempty"`
	MessageCount       int64           `json:"message_count,omitempty"`
	ToolCallCount      int64           `json:"tool_call_count,omitempty"`
}

func (p *Provider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*hooks.Event, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}

	var payload stdinPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse stdin JSON: %w", err)
	}

	transcriptRef := resolveTranscriptRef(payload)
	cwd := payload.CWD
	if cwd == "" {
		cwd = firstWorkspaceRoot(payload.WorkspaceRoots)
	}

	event := &hooks.Event{
		SessionID:     payload.ConversationID,
		TranscriptRef: transcriptRef,
		Prompt:        payload.Prompt,
		Model:         payload.Model,
		Timestamp:     time.Now().UnixMilli(),
		CWD:           cwd,
	}

	switch hookName {
	case "before-submit-prompt":
		event.Type = hooks.PromptSubmitted
	case "pre-tool-use":
		if normalizeCursorToolName(payload.ToolName) != "Agent" {
			return nil, nil
		}
		event.Type = hooks.SubagentPromptSubmitted
		event.ToolName = "Agent"
		event.ToolUseID = normalizeCursorToolUseID(payload.ToolUseID)
		event.ToolInput = bytes.TrimSpace(data)
	case "post-tool-use":
		if normalizeCursorToolName(payload.ToolName) != "Bash" {
			return nil, nil
		}
		event.Type = hooks.ToolStepCompleted
		event.ToolName = "Bash"
		event.ToolUseID = normalizeCursorToolUseID(payload.ToolUseID)
		event.ToolInput = bytes.TrimSpace(data)
	case "after-file-edit":
		toolName := classifyCursorEdit(payload.Edits)
		if toolName == "" {
			return nil, nil
		}
		event.Type = hooks.ToolStepCompleted
		event.ToolName = toolName
		event.ToolUseID = syntheticCursorToolUseID(payload.GenerationID, data)
		event.ToolInput = bytes.TrimSpace(data)
	case "stop":
		event.Type = hooks.AgentCompleted
	case "subagent-stop":
		event.Type = hooks.SubagentCompleted
		event.SubagentID = normalizeCursorToolUseID(payload.SubagentID)
		event.ToolName = "Agent"
		event.ToolUseID = normalizeCursorToolUseID(payload.SubagentID)
		event.ToolInput = bytes.TrimSpace(data)
	case "session-start":
		event.Type = hooks.SessionOpened
	case "session-end":
		event.Type = hooks.SessionClosed
	case "pre-compact":
		event.Type = hooks.ContextCompacted
	default:
		return nil, nil
	}

	return event, nil
}

type cursorEdit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func firstWorkspaceRoot(roots []string) string {
	for _, root := range roots {
		if root != "" {
			return root
		}
	}
	return ""
}

func resolveTranscriptRef(payload stdinPayload) string {
	if payload.TranscriptPath != "" {
		return payload.TranscriptPath
	}
	if envPath := os.Getenv("CURSOR_TRANSCRIPT_PATH"); envPath != "" {
		return envPath
	}
	workspaceRoot := firstWorkspaceRoot(payload.WorkspaceRoots)
	if workspaceRoot == "" {
		workspaceRoot = os.Getenv("CURSOR_PROJECT_DIR")
	}
	if workspaceRoot == "" || payload.ConversationID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cleanRoot := filepath.Clean(workspaceRoot)
	encoded := strings.TrimPrefix(filepath.ToSlash(cleanRoot), "/")
	encoded = strings.ReplaceAll(encoded, "/", "-")
	if encoded == "" {
		return ""
	}
	return filepath.Join(
		home,
		".cursor",
		"projects",
		encoded,
		"agent-transcripts",
		payload.ConversationID,
		payload.ConversationID+".jsonl",
	)
}

func normalizeCursorToolName(name string) string {
	switch strings.TrimSpace(name) {
	case "Shell":
		return "Bash"
	case "Subagent":
		return "Agent"
	default:
		return strings.TrimSpace(name)
	}
}

func normalizeCursorToolUseID(toolUseID string) string {
	fields := strings.Fields(toolUseID)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func classifyCursorEdit(edits []cursorEdit) string {
	if len(edits) == 0 {
		return ""
	}
	if len(edits) == 1 && edits[0].OldString == "" {
		return "Write"
	}
	return "Edit"
}

func syntheticCursorToolUseID(seed string, raw []byte) string {
	hh := sha256.New()
	if seed != "" {
		hh.Write([]byte(seed))
		hh.Write([]byte{':'})
	}
	hh.Write(bytes.TrimSpace(raw))
	return "cursor-step-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	f, err := os.Open(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}

func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	f, err := os.Open(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	// Extract conversation ID from path for session linking.
	convID := extractConversationID(transcriptRef)
	parentConvID := extractParentConversationID(transcriptRef)

	// Decode project path for no-path fallback routing.
	projectPath := agentcursor.DecodeProjectPathFromSourceKey(transcriptRef)

	model := hooks.ModelFromContext(ctx)

	meta := map[string]any{"source_key": transcriptRef}
	if projectPath != "" {
		meta["project_path"] = projectPath
	}
	metaJSON, _ := json.Marshal(meta)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	lineNum := 0
	var events []broker.RawEvent
	eventTs := time.Now().UnixMilli()

	for scanner.Scan() {
		if lineNum < offset {
			lineNum++
			continue
		}

		raw := scanner.Bytes()
		line := strings.TrimSpace(string(raw))
		if line == "" {
			lineNum++
			continue
		}

		bd := agentcursor.ParseCursorJSONLLine(line)
		if bd.Role == "" {
			lineNum++
			continue
		}

		// Content-addressed event ID.
		hh := sha256.New()
		hh.Write([]byte(transcriptRef))
		_, _ = fmt.Fprintf(hh, ":%d:", lineNum)
		hh.Write(bytes.TrimRight(raw, "\r\n"))
		eventID := hex.EncodeToString(hh.Sum(nil))

		var payloadHash string
		if bs != nil {
			payloadHash, _, _ = bs.Put(ctx, bytes.TrimRight(raw, "\r\n"))
		}

		toolUsesStr := ""
		if s := agentcursor.SerializeToolUses(bd.ToolUses, bd.ContentTypes); s.Valid {
			toolUsesStr = s.String
		}

		filePaths := broker.ExtractFilePaths(toolUsesStr)

		events = append(events, broker.RawEvent{
			EventID:           eventID,
			SourceKey:         transcriptRef,
			Provider:          agentcursor.ProviderName,
			SourcePosition:    int64(lineNum),
			Timestamp:         eventTs,
			Kind:              bd.Kind,
			Role:              bd.Role,
			ToolUsesJSON:      toolUsesStr,
			Summary:           bd.Summary,
			PayloadHash:       payloadHash,
			FilePaths:         filePaths,
			ProviderSessionID: convID,
			ParentSessionID:   parentConvID,
			SessionStartedAt:  eventTs,
			SessionMetaJSON:   string(metaJSON),
			SourceProjectPath: projectPath,
			Model:             model,
		})

		eventTs++
		lineNum++
	}

	// Enrich events that lack file paths with data from Cursor's
	// ai-code-tracking.db. The IDE Composer doesn't write tool_use blocks
	// to the JSONL transcript, but records every touched file in
	// ai_code_hashes keyed by conversation ID.
	if len(events) > 0 {
		// Read the capture state timestamp from context to scope enrichment
		// to the current turn (avoids smearing files from earlier turns).
		var sinceTs int64
		if ts, ok := ctx.Value(hooks.CaptureTimestampKey).(int64); ok {
			sinceTs = ts
		}
		enrichFromCodeHashes(events, convID, sinceTs)
	}

	return events, lineNum, scanner.Err()
}

// DiscoverSubagentTranscripts scans the parent transcript's subagents/
// directory and returns any child JSONL transcripts.
func (p *Provider) DiscoverSubagentTranscripts(ctx context.Context, parentTranscriptRef string) ([]string, error) {
	subagentsDir := filepath.Join(filepath.Dir(parentTranscriptRef), "subagents")

	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read subagents dir: %w", err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(subagentsDir, entry.Name()))
	}
	return paths, nil
}

// SubagentStateKey returns a stable key for a Cursor subagent transcript.
func (p *Provider) SubagentStateKey(subagentTranscriptRef string) string {
	base := filepath.Base(subagentTranscriptRef)
	return strings.TrimSuffix(base, ".jsonl")
}

// enrichFromCodeHashes queries Cursor's ai-code-tracking.db for file paths
// associated with the given conversation ID, then distributes those paths
// across assistant events that have no FilePaths of their own.
//
// This bridges the gap where Cursor IDE transcripts are text-only but the
// IDE still records file associations for the conversation.
//
// sinceTs (unix ms) scopes the query to hashes created after the last capture,
// preventing files from earlier turns being smeared onto the current turn.
// Pass 0 to disable filtering (first capture of a conversation).
//
// File paths are stored as absolute in ToolUsesJSON. WriteEventsToRepo
// relativizes them per target repo so cross-repo routing works correctly.
func enrichFromCodeHashes(events []broker.RawEvent, conversationID string, sinceTs int64) {
	if conversationID == "" {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(home, ".cursor", "ai-tracking", "ai-code-tracking.db")

	// Best-effort: if DB doesn't exist or can't be read, skip silently.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	var query string
	var args []any
	if sinceTs > 0 {
		query = "SELECT DISTINCT fileName FROM ai_code_hashes WHERE conversationId = ? AND fileName != '' AND timestamp >= ?"
		args = []any{conversationID, sinceTs}
	} else {
		query = "SELECT DISTINCT fileName FROM ai_code_hashes WHERE conversationId = ? AND fileName != ''"
		args = []any{conversationID}
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		slog.Debug("cursor: ai_code_hashes query failed", "err", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var filePaths []string
	seen := make(map[string]bool)
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			continue
		}
		cleaned := filepath.Clean(fp)
		if filepath.IsAbs(cleaned) && !seen[cleaned] {
			seen[cleaned] = true
			filePaths = append(filePaths, cleaned)
		}
	}

	if len(filePaths) == 0 {
		return
	}

	// Build synthetic tool_uses JSON with absolute paths. The attribution
	// system checks for "cursor_file_edit" tool name. Paths are kept
	// absolute here; WriteEventsToRepo relativizes per target repo.
	type tool struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path"`
		FileOp   string `json:"file_op"`
	}
	type payload struct {
		ContentTypes []string `json:"content_types"`
		Tools        []tool   `json:"tools"`
	}
	var tools []tool
	for _, fp := range filePaths {
		tools = append(tools, tool{Name: "cursor_file_edit", FilePath: fp, FileOp: "edit"})
	}
	syntheticJSON, _ := json.Marshal(payload{
		ContentTypes: []string{"text", "tool_use"},
		Tools:        tools,
	})
	synthetic := string(syntheticJSON)

	// Distribute to assistant events that have no paths from CLI tool_use
	// blocks. Both FilePaths (routing) and ToolUsesJSON (attribution) use
	// absolute paths at this stage.
	for i := range events {
		if events[i].Role == "assistant" && len(events[i].FilePaths) == 0 {
			events[i].FilePaths = filePaths
			events[i].ToolUsesJSON = synthetic
		}
	}
}

// extractConversationID extracts a conversation/session ID from a Cursor
// agent-transcripts path. The format is typically:
// ~/.cursor/agent-transcripts/<conversation-id>.jsonl
func extractConversationID(path string) string {
	base := strings.TrimSuffix(path, ".jsonl")
	return base[strings.LastIndex(base, "/")+1:]
}

func extractParentConversationID(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "subagents" {
		return ""
	}
	return filepath.Base(filepath.Dir(dir))
}
