package kirocli

import (
	"context"
	"crypto/sha256"
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

	// sessionsDir overrides ~/.kiro/sessions/cli for tests.
	sessionsDir string
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

const (
	agentName        = "semantica"
	agentDescription = "Semantica capture wrapper. Mirrors default-agent behavior and emits attribution events."
)

// agentToolsWildcard is the value used for the "tools" field. The
// wildcard makes the wrapper agent usable for any task without
// enumerating every built-in tool, which would drift as the host CLI
// adds new tools.
var agentToolsWildcard = []string{"*"}

// hookEntries returns the canonical Kiro CLI hooks. File and shell
// tools use post hooks only; subagents use pre and post boundaries.
func hookEntries(bin string) []struct {
	event   string
	matcher string
	command string
	timeout int
} {
	return []struct {
		event   string
		matcher string
		command string
		timeout int
	}{
		{"agentSpawn", "", hooks.GuardedCommand(bin, "capture kiro-cli agent-spawn"), 5000},
		{"userPromptSubmit", "", hooks.GuardedCommand(bin, "capture kiro-cli user-prompt-submit"), 10000},
		{"preToolUse", "subagent", hooks.GuardedCommand(bin, "capture kiro-cli pre-tool-use"), 10000},
		{"postToolUse", "fs_write", hooks.GuardedCommand(bin, "capture kiro-cli post-tool-use"), 10000},
		{"postToolUse", "execute_bash", hooks.GuardedCommand(bin, "capture kiro-cli post-tool-use"), 10000},
		{"postToolUse", "subagent", hooks.GuardedCommand(bin, "capture kiro-cli post-tool-use"), 10000},
		{"stop", "", hooks.GuardedCommand(bin, "capture kiro-cli stop"), 60000},
	}
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

	// Refresh Semantica-owned top-level fields on every install so
	// stale values from earlier writes are corrected. User-added
	// fields like prompt or model pass through untouched because
	// the surrounding raw map is preserved.
	nameJSON, _ := json.Marshal(agentName)
	raw["name"] = nameJSON
	descriptionJSON, _ := json.Marshal(agentDescription)
	raw["description"] = descriptionJSON
	toolsJSON, _ := json.Marshal(agentToolsWildcard)
	raw["tools"] = toolsJSON

	var existingHooks map[string][]agentConfigHookEntry
	if hooksRaw, ok := raw["hooks"]; ok {
		_ = json.Unmarshal(hooksRaw, &existingHooks)
	}
	if existingHooks == nil {
		existingHooks = make(map[string][]agentConfigHookEntry)
	}

	// Strip every existing Semantica-marked entry before appending
	// the canonical set. This protects against stale entries from
	// earlier installs that registered different events or used a
	// different matcher (or no matcher at all): without this pass,
	// an old unmatched postToolUse hook would survive alongside the
	// new fs_write / execute_bash rows and fire for every tool,
	// duplicating capture. User-added entries that do not contain
	// the marker are preserved.
	for event, entries := range existingHooks {
		var kept []agentConfigHookEntry
		for _, e := range entries {
			if !strings.Contains(e.Command, semanticaMarker) {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			existingHooks[event] = kept
		} else {
			delete(existingHooks, event)
		}
	}

	count := 0
	for _, def := range hookEntries(bin) {
		existingHooks[def.event] = append(existingHooks[def.event], agentConfigHookEntry{
			Command:   def.command,
			TimeoutMs: def.timeout,
			Matcher:   def.matcher,
		})
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

	// Print a one-line activation hint. Hooks fire only for sessions
	// that select this agent, so the user has to opt in explicitly
	// the first time.
	fmt.Fprintln(os.Stderr, "Kiro CLI hooks installed. To activate capture, run: kiro-cli agent set-default semantica")

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
		if subagentTools[payload.ToolName] {
			turnID := loadTurnIDFromCaptureState(wsKey)
			event.ToolUseID = syntheticSubagentToolUseID(payload.SessionID, turnID, payload.ToolName, payload.ToolInput)
			event.Type = hooks.SubagentCompleted
		} else {
			event.ToolUseID = syntheticToolUseID(wsKey, event.Timestamp, payload.ToolName, payload.ToolInput)
			event.Type = hooks.ToolStepCompleted
		}
		return event, nil
	case "pre-tool-use":
		if !subagentTools[payload.ToolName] {
			return nil, nil
		}
		event.Type = hooks.SubagentPromptSubmitted
		event.ToolName = "Agent"
		turnID := loadTurnIDFromCaptureState(wsKey)
		event.ToolUseID = syntheticSubagentToolUseID(payload.SessionID, turnID, payload.ToolName, payload.ToolInput)
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

	// Conversation lookup is best-effort. Direct postToolUse hooks
	// own capture, so prompt events must still save capture state
	// even when the local conversation store is unavailable.
	resolve := p.resolveConversation
	if resolve == nil {
		resolve = resolveLatestConversation
	}
	dbPath, convID, err := resolve(cwd)
	if err != nil {
		return event, nil
	}

	event.TranscriptRef = buildTranscriptRef(dbPath, convID)

	// The offset marker is advisory while replay emission is disabled.
	// Skip it if the conversation cannot be read.
	if event.Type == hooks.PromptSubmitted {
		loadFn := p.loadConv
		if loadFn == nil {
			loadFn = loadConversation
		}
		conv, err := loadFn(dbPath, convID)
		if err != nil {
			return event, nil
		}
		calls := extractToolCalls(conv)
		lastID := ""
		if len(calls) > 0 {
			lastID = calls[len(calls)-1].ID
		}
		_ = writeSidecar(wsKey, lastID)
	}

	return event, nil
}

// normalizeKiroToolName maps Kiro CLI hook tool names to canonical
// Semantica tool names. The write tool covers three sub-commands
// (create, strReplace, insert) keyed off tool_input.command;
// strReplace and insert both map to Edit because they produce a
// before-and-after string pair that the scorer treats uniformly.
// The subagent tool is the AgentCrew dispatcher and maps to Agent.
// An empty return drops the event.
func normalizeKiroToolName(toolName string, toolInput json.RawMessage) string {
	switch toolName {
	case "write":
		var inp fsWriteInput
		if json.Unmarshal(toolInput, &inp) == nil {
			switch inp.Command {
			case "create":
				return "Write"
			case "strReplace", "insert":
				return "Edit"
			}
		}
		return ""
	case "shell":
		return "Bash"
	case "subagent":
		return "Agent"
	default:
		return ""
	}
}

// loadTurnIDFromCaptureState returns the active turn id, if one has
// already been saved for the workspace.
func loadTurnIDFromCaptureState(wsKey string) string {
	state, err := hooks.LoadCaptureStateByKey(wsKey)
	if err != nil {
		return ""
	}
	return state.TurnID
}

func syntheticToolUseID(wsKey string, ts int64, toolName string, toolInput json.RawMessage) string {
	hh := sha256.New()
	hh.Write([]byte(wsKey))
	_, _ = fmt.Fprintf(hh, ":%d:%s:", ts, toolName)
	hh.Write(toolInput)
	return "kiro-step-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

// syntheticSubagentToolUseID derives the shared pre/post id for a
// subagent dispatch. Kiro hook payloads do not expose a provider tool
// id, so the hash uses the Kiro session, Semantica turn, tool name,
// and tool input. Omitting timestamp lets pre/post pair; including
// turn id keeps identical dispatches in later prompts distinct.
func syntheticSubagentToolUseID(sessionID, turnID, toolName string, toolInput json.RawMessage) string {
	hh := sha256.New()
	hh.Write([]byte(sessionID))
	_, _ = fmt.Fprintf(hh, ":%s:%s:", turnID, toolName)
	hh.Write(toolInput)
	return "kiro-subagent-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

// TranscriptOffset reports the current file-write count when the
// conversation store can be read. Replay is disabled for Kiro CLI, so
// unreadable or missing transcript refs degrade to offset 0.
func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	if transcriptRef == "" {
		return 0, nil
	}
	dbPath, convID, err := parseTranscriptRef(transcriptRef)
	if err != nil {
		return 0, nil
	}
	conv, err := loadConversation(dbPath, convID)
	if err != nil {
		return 0, nil
	}
	return len(extractToolCalls(conv)), nil
}

// ReadFromOffset branches by transcript-ref shape. SQLite composite refs
// (parent) stay a no-op because direct postToolUse hooks own parent capture
// and replay would duplicate. JSONL refs (subagent children supplied by the
// discoverer) are replayed since no direct-hook surface exists for inner
// stage edits.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	if looksLikeKiroChildJSONLRef(transcriptRef) {
		return readChildJSONL(ctx, transcriptRef, offset, bs)
	}
	if transcriptRef == "" {
		return nil, offset, nil
	}
	dbPath, convID, err := parseTranscriptRef(transcriptRef)
	if err != nil {
		return nil, offset, nil
	}
	conv, err := loadConversation(dbPath, convID)
	if err != nil {
		return nil, offset, nil
	}
	return nil, len(extractToolCalls(conv)), nil
}
