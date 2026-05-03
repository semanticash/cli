package kirocli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
)

var providerName = agentKiro.ProviderNameCLI

// Provider implements hooks.HookProvider for Kiro CLI.
type Provider struct {
	// resolveConversation overrides conversation lookup in tests.
	resolveConversation func(workspacePath string) (dbPath, conversationID string, err error)

	// loadConv overrides conversation loading in tests.
	loadConv func(dbPath, conversationID string) (*conversationValue, error)
}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "Kiro CLI" }

func (p *Provider) IsAvailable() bool {
	return util.ResolveExecutable([]string{"kiro-cli", "kiro"}) != ""
}

const semanticaMarker = "semantica capture kiro-cli"

type agentConfigHookEntry struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Matcher   string `json:"matcher,omitempty"`
}

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	agentsDir := filepath.Join(repoRoot, ".kiro", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return 0, fmt.Errorf("create .kiro/agents: %w", err)
	}

	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	configPath := filepath.Join(agentsDir, "semantica.json")

	var raw map[string]json.RawMessage
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return 0, fmt.Errorf("parse existing agent config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}

	if _, ok := raw["name"]; !ok {
		nameJSON, _ := json.Marshal("semantica")
		raw["name"] = nameJSON
	}

	if _, ok := raw["tools"]; !ok {
		toolsJSON, _ := json.Marshal([]string{
			"read", "write", "shell", "aws", "report", "introspect",
			"knowledge", "thinking", "todo", "delegate", "grep", "glob",
		})
		raw["tools"] = toolsJSON
	}

	var existingHooks map[string][]agentConfigHookEntry
	if hooksRaw, ok := raw["hooks"]; ok {
		_ = json.Unmarshal(hooksRaw, &existingHooks)
	}
	if existingHooks == nil {
		existingHooks = make(map[string][]agentConfigHookEntry)
	}

	hookDefs := []struct {
		event   string
		command string
		timeout int
	}{
		{"userPromptSubmit", hooks.GuardedCommand(bin, "capture kiro-cli user-prompt-submit"), 10000},
		{"stop", hooks.GuardedCommand(bin, "capture kiro-cli stop"), 60000},
		{"postToolUse", hooks.GuardedCommand(bin, "capture kiro-cli post-tool-use"), 10000},
		{"preToolUse", hooks.GuardedCommand(bin, "capture kiro-cli pre-tool-use"), 10000},
		{"agentSpawn", hooks.GuardedCommand(bin, "capture kiro-cli agent-spawn"), 5000},
	}

	count := 0
	for _, def := range hookDefs {
		entries := existingHooks[def.event]
		found := false
		for _, e := range entries {
			if strings.Contains(e.Command, semanticaMarker) {
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, agentConfigHookEntry{
				Command:   def.command,
				TimeoutMs: def.timeout,
			})
			existingHooks[def.event] = entries
		}
		count++
	}

	hooksJSON, _ := hooks.MarshalCompactJSON(existingHooks)
	raw["hooks"] = hooksJSON

	data, err := hooks.MarshalSettingsJSON(raw)
	if err != nil {
		return 0, fmt.Errorf("marshal agent config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return 0, fmt.Errorf("write agent config: %w", err)
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	configPath := filepath.Join(repoRoot, ".kiro", "agents", "semantica.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var hooksMap map[string][]agentConfigHookEntry
	if hooksRaw, ok := raw["hooks"]; ok {
		_ = json.Unmarshal(hooksRaw, &hooksMap)
	}
	if hooksMap == nil {
		return nil
	}

	for event, entries := range hooksMap {
		var kept []agentConfigHookEntry
		for _, e := range entries {
			if !strings.Contains(e.Command, semanticaMarker) {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			hooksMap[event] = kept
		} else {
			delete(hooksMap, event)
		}
	}

	hooksJSON, _ := hooks.MarshalCompactJSON(hooksMap)
	raw["hooks"] = hooksJSON

	out, err := hooks.MarshalSettingsJSON(raw)
	if err != nil {
		return fmt.Errorf("marshal agent config: %w", err)
	}
	return os.WriteFile(configPath, out, 0o644)
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	configPath := filepath.Join(repoRoot, ".kiro", "agents", "semantica.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), semanticaMarker)
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	configPath := filepath.Join(repoRoot, ".kiro", "agents", "semantica.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read agent config: %w", err)
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return "", fmt.Errorf("parse agent config")
	}
	var hooksMap map[string][]agentConfigHookEntry
	if hooksRaw, ok := raw["hooks"]; ok {
		_ = json.Unmarshal(hooksRaw, &hooksMap)
	}
	for _, entries := range hooksMap {
		for _, e := range entries {
			if strings.Contains(e.Command, semanticaMarker) {
				if bin := hooks.ExtractBinary(e.Command); bin != "" {
					return bin, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hooks found")
}

func workspaceKey(absPath string) string {
	return agentKiro.WorkspaceKey("kirocli", absPath)
}

// ParseHookEvent parses the Kiro CLI hook payload.
func (p *Provider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*hooks.Event, error) {
	var payload hookPayload
	if stdin != nil {
		if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
			return nil, fmt.Errorf("parse kiro-cli payload: %w", err)
		}
	}

	cwd := payload.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve cwd: %w", err)
		}
	}

	wsKey := workspaceKey(cwd)

	event := &hooks.Event{
		SessionID:    wsKey,
		Prompt:       payload.Prompt,
		Timestamp:    time.Now().UnixMilli(),
		CWD:          cwd,
		ToolName:     payload.ToolName,
		ToolInput:    payload.ToolInput,
		ToolResponse: payload.ToolResponse,
	}

	switch hookName {
	case "user-prompt-submit":
		event.Type = hooks.PromptSubmitted
	case "stop":
		event.Type = hooks.AgentCompleted
	case "agent-spawn":
		event.Type = hooks.SessionOpened
		return event, nil
	case "post-tool-use":
		toolName := normalizeKiroToolName(payload.ToolName, payload.ToolInput)
		if toolName == "" {
			return nil, nil
		}
		event.ToolName = toolName
		event.ToolUseID = syntheticToolUseID(wsKey, event.Timestamp, payload.ToolName, payload.ToolInput)
		if subagentTools[payload.ToolName] {
			event.Type = hooks.SubagentCompleted
		} else {
			event.Type = hooks.ToolStepCompleted
		}
		return event, nil
	case "pre-tool-use":
		if !subagentTools[payload.ToolName] {
			return nil, nil
		}
		event.Type = hooks.SubagentPromptSubmitted
		event.ToolName = "Agent"
		event.ToolUseID = syntheticToolUseID(wsKey, event.Timestamp, payload.ToolName, payload.ToolInput)
		return event, nil
	default:
		return nil, nil
	}

	// Reuse the pinned conversation reference when it is available.
	if event.Type == hooks.AgentCompleted {
		if state, err := hooks.LoadCaptureStateByKey(wsKey); err == nil {
			event.TranscriptRef = state.TranscriptRef
			return event, nil
		}
	}

	resolve := p.resolveConversation
	if resolve == nil {
		resolve = resolveLatestConversation
	}
	dbPath, convID, err := resolve(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve kiro-cli session: %w", err)
	}

	transcriptRef := buildTranscriptRef(dbPath, convID)
	event.TranscriptRef = transcriptRef

	// Record the current fs_write boundary before lifecycle state is saved.
	if event.Type == hooks.PromptSubmitted {
		loadFn := p.loadConv
		if loadFn == nil {
			loadFn = loadConversation
		}
		conv, err := loadFn(dbPath, convID)
		if err != nil {
			return nil, fmt.Errorf("load conversation for offset: %w", err)
		}
		calls := extractToolCalls(conv)
		lastID := ""
		if len(calls) > 0 {
			lastID = calls[len(calls)-1].ID
		}
		if err := writeSidecar(wsKey, lastID); err != nil {
			return nil, fmt.Errorf("write offset sidecar: %w", err)
		}
	}

	return event, nil
}

// normalizeKiroToolName maps Kiro CLI tool names to normalized Semantica tool names.
func normalizeKiroToolName(toolName string, toolInput json.RawMessage) string {
	switch toolName {
	case "fs_write":
		var inp fsWriteInput
		if json.Unmarshal(toolInput, &inp) == nil {
			switch inp.Command {
			case "create":
				return "Write"
			case "str_replace":
				return "Edit"
			}
		}
		return "Write" // default for unknown fs_write commands
	case "execute_bash":
		return "Bash"
	case "use_subagent", "delegate":
		return "Agent"
	default:
		return ""
	}
}

func syntheticToolUseID(wsKey string, ts int64, toolName string, toolInput json.RawMessage) string {
	hh := sha256.New()
	hh.Write([]byte(wsKey))
	_, _ = fmt.Fprintf(hh, ":%d:%s:", ts, toolName)
	hh.Write(toolInput)
	return "kiro-step-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

// TranscriptOffset returns the current number of file-writing tool calls in
// the conversation. ReadFromOffset uses the sidecar marker to select new
// calls on the stop hook.
func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	dbPath, convID, err := parseTranscriptRef(transcriptRef)
	if err != nil {
		return 0, err
	}
	conv, err := loadConversation(dbPath, convID)
	if err != nil {
		return 0, err
	}
	return len(extractToolCalls(conv)), nil
}

// ReadFromOffset returns file-writing tool calls that occur after the saved
// sidecar marker.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	dbPath, convID, err := parseTranscriptRef(transcriptRef)
	if err != nil {
		return nil, offset, err
	}

	conv, err := loadConversation(dbPath, convID)
	if err != nil {
		return nil, offset, nil
	}

	allCalls := extractToolCalls(conv)

	// The conversation key stores the workspace path.
	wsKey := ""
	db, dbErr := openReadOnly(dbPath)
	if dbErr == nil {
		var key string
		_ = db.QueryRow(
			`SELECT key FROM conversations_v2 WHERE conversation_id = ?`, convID,
		).Scan(&key)
		_ = db.Close()
		if key != "" {
			wsKey = workspaceKey(key)
		}
	}

	lastSeenID := ""
	if wsKey != "" {
		lastSeenID, _ = readSidecar(wsKey)
	}

	newCalls := newToolCallsSince(allCalls, lastSeenID)

	workspacePath := ""
	if db2, err := openReadOnly(dbPath); err == nil {
		_ = db2.QueryRow(
			`SELECT key FROM conversations_v2 WHERE conversation_id = ?`, convID,
		).Scan(&workspacePath)
		_ = db2.Close()
	}

	events := toolCallsToEvents(newCalls, transcriptRef, workspacePath, convID, time.Now().UnixMilli())

	// The sidecar is written at prompt submission. Leaving it unchanged here
	// allows the same events to be retried if routing fails.

	newOffset := len(allCalls)
	return events, newOffset, nil
}

func openReadOnly(dbPath string) (*sql.DB, error) {
	return sql.Open("sqlite", dbPath+"?mode=ro")
}
