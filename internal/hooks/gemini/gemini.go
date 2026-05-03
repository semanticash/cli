package gemini

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
	agentgemini "github.com/semanticash/cli/internal/agents/gemini"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
)

const providerName = "gemini-cli"

// Provider implements hooks.HookProvider for Gemini CLI.
type Provider struct{}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "Gemini CLI" }

func (p *Provider) IsAvailable() bool {
	return util.ResolveExecutable([]string{"gemini"}) != ""
}

const semanticaMarker = "semantica capture gemini-cli"

type geminiHookMatcher struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []geminiHookEntry `json:"hooks"`
}

type geminiHookEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Command string `json:"command"`
}

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	settingsPath := filepath.Join(repoRoot, ".gemini", "settings.json")

	var raw map[string]json.RawMessage
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return 0, fmt.Errorf("parse existing %s: %w", settingsPath, err)
		}
	}
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}

	// Ensure hooksConfig.enabled = true.
	hooksConfigJSON, err := json.Marshal(map[string]bool{"enabled": true})
	if err != nil {
		return 0, fmt.Errorf("marshal hooksConfig: %w", err)
	}
	raw["hooksConfig"] = hooksConfigJSON

	existingHooks := make(map[string][]geminiHookMatcher)
	if h, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(h, &existingHooks); err != nil {
			return 0, fmt.Errorf("parse hooks in %s: %w", settingsPath, err)
		}
	}

	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	hookDefs := []struct {
		hookPoint string
		matcher   string
		name      string
		command   string
	}{
		{"BeforeAgent", "", "semantica-before-agent", hooks.GuardedCommand(bin, "capture gemini-cli before-agent")},
		{"AfterAgent", "", "semantica-after-agent", hooks.GuardedCommand(bin, "capture gemini-cli after-agent")},
		{"SessionStart", "", "semantica-session-start", hooks.GuardedCommand(bin, "capture gemini-cli session-start")},
		{"SessionEnd", "exit", "semantica-session-end-exit", hooks.GuardedCommand(bin, "capture gemini-cli session-end")},
		{"SessionEnd", "logout", "semantica-session-end-logout", hooks.GuardedCommand(bin, "capture gemini-cli session-end")},
		{"PreCompress", "", "semantica-pre-compress", hooks.GuardedCommand(bin, "capture gemini-cli pre-compress")},
		{"BeforeModel", "", "semantica-before-model", hooks.GuardedCommand(bin, "capture gemini-cli before-model")},
		{"BeforeTool", "*", "semantica-before-tool", hooks.GuardedCommand(bin, "capture gemini-cli before-tool")},
		{"AfterTool", "*", "semantica-after-tool", hooks.GuardedCommand(bin, "capture gemini-cli after-tool")},
	}

	// Skip entries whose name already exists. This treats a hand-edited
	// hook as intentional: if the user (or a debugging workflow) put a
	// custom command under our name, leave it alone. Resetting to the
	// canonical form is done by `disable` followed by `enable` since
	// `disable` removes our entries by marker.
	count := 0
	for _, def := range hookDefs {
		matchers := existingHooks[def.hookPoint]
		nameExists := false
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if h.Name == def.name {
					nameExists = true
					break
				}
			}
			if nameExists {
				break
			}
		}
		if !nameExists {
			existingHooks[def.hookPoint] = append(matchers, geminiHookMatcher{
				Matcher: def.matcher,
				Hooks:   []geminiHookEntry{{Name: def.name, Type: "command", Command: def.command}},
			})
		}
		count++
	}

	hooksJSON, _ := hooks.MarshalCompactJSON(existingHooks)
	raw["hooks"] = hooksJSON

	out, err := hooks.MarshalSettingsJSON(raw)
	if err != nil {
		return 0, fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir .gemini: %w", err)
	}
	if err := os.WriteFile(settingsPath, out, 0o644); err != nil {
		return 0, fmt.Errorf("write settings: %w", err)
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	settingsPath := filepath.Join(repoRoot, ".gemini", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	hooksRaw, ok := raw["hooks"]
	if !ok {
		return nil
	}

	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(hooksRaw, &hooksMap); err != nil {
		return nil
	}

	for hookPoint, matchers := range hooksMap {
		var kept []geminiHookMatcher
		for _, m := range matchers {
			var keptHooks []geminiHookEntry
			for _, h := range m.Hooks {
				if !strings.Contains(h.Command, semanticaMarker) {
					keptHooks = append(keptHooks, h)
				}
			}
			if len(keptHooks) > 0 {
				m.Hooks = keptHooks
				kept = append(kept, m)
			}
		}
		if len(kept) > 0 {
			hooksMap[hookPoint] = kept
		} else {
			delete(hooksMap, hookPoint)
		}
	}

	hooksJSON, _ := hooks.MarshalCompactJSON(hooksMap)
	raw["hooks"] = hooksJSON
	out, _ := hooks.MarshalSettingsJSON(raw)
	return os.WriteFile(settingsPath, out, 0o644)
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gemini", "settings.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), semanticaMarker)
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gemini", "settings.json"))
	if err != nil {
		return "", err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", err
	}
	hooksRaw, ok := raw["hooks"]
	if !ok {
		return "", fmt.Errorf("no hooks in settings")
	}
	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(hooksRaw, &hooksMap); err != nil {
		return "", err
	}
	for _, matchers := range hooksMap {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					if bin := hooks.ExtractBinary(h.Command); bin != "" {
						return bin, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hook found")
}

// stdinPayload is the JSON structure sent by Gemini CLI hooks on stdin.
type stdinPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Prompt         string          `json:"prompt,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Timestamp      string          `json:"timestamp,omitempty"`
	HookEventName  string          `json:"hook_event_name,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	LLMRequest     json.RawMessage `json:"llm_request,omitempty"`
}

// stateAlteringTools are Gemini tool names captured as direct step events.
var stateAlteringTools = map[string]bool{
	"write_file":        true,
	"edit_file":         true,
	"replace":           true,
	"save_file":         true,
	"run_shell_command": true,
}

// subagentTools are Gemini tool names for subagent delegation.
// Gemini 0.40+ dispatches subagents through invoke_agent.
var subagentTools = map[string]bool{
	"invoke_agent": true,
}

// inferGeminiToolName returns the hook tool name, falling back to
// tool_input shape when Gemini omits tool_name.
func inferGeminiToolName(toolName string, toolInput json.RawMessage) string {
	if toolName != "" {
		return toolName
	}
	if len(toolInput) == 0 {
		return ""
	}
	var probe struct {
		Content   *string `json:"content"`
		OldString *string `json:"old_string"`
		NewString *string `json:"new_string"`
		FilePath  *string `json:"file_path"`
		Command   *string `json:"command"`
	}
	if err := json.Unmarshal(toolInput, &probe); err != nil {
		return ""
	}
	switch {
	case probe.OldString != nil && probe.NewString != nil && probe.FilePath != nil:
		return "replace"
	case probe.Content != nil && probe.FilePath != nil:
		return "write_file"
	case probe.Command != nil:
		return "run_shell_command"
	}
	return ""
}

// parseTimestamp parses a Gemini CLI ISO 8601 timestamp string into unix
// milliseconds. Falls back to time.Now() if the string is empty or invalid.
func parseTimestamp(s string) int64 {
	if s == "" {
		return time.Now().UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return time.Now().UnixMilli()
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

	event := &hooks.Event{
		SessionID:     payload.SessionID,
		TranscriptRef: payload.TranscriptPath,
		Prompt:        payload.Prompt,
		Timestamp:     parseTimestamp(payload.Timestamp),
		CWD:           payload.CWD,
		ToolName:      payload.ToolName,
		ToolInput:     payload.ToolInput,
		ToolResponse:  payload.ToolResponse,
	}

	switch hookName {
	case "before-agent":
		event.Type = hooks.PromptSubmitted
	case "after-agent":
		event.Type = hooks.AgentCompleted
	case "session-start":
		event.Type = hooks.SessionOpened
	case "session-end":
		event.Type = hooks.SessionClosed
	case "pre-compress":
		event.Type = hooks.ContextCompacted
	case "before-model":
		// Extract model from llm_request and store on event for context propagation.
		if len(payload.LLMRequest) > 0 {
			var req struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(payload.LLMRequest, &req) == nil && req.Model != "" {
				event.Model = req.Model
			}
		}
		// BeforeModel has no lifecycle action - model is captured via context.
		return nil, nil
	case "before-tool":
		// Some Gemini payloads omit tool_name; infer it from tool_input.
		resolvedTool := inferGeminiToolName(payload.ToolName, payload.ToolInput)
		event.ToolName = resolvedTool
		if subagentTools[resolvedTool] {
			event.Type = hooks.SubagentPromptSubmitted
			event.ToolName = "Agent"
			event.ToolUseID = syntheticToolUseID(payload.SessionID, event.Timestamp, resolvedTool, payload.ToolInput)
		} else {
			// Non-subagent before-tool has no lifecycle action.
			return nil, nil
		}
	case "after-tool":
		resolvedTool := inferGeminiToolName(payload.ToolName, payload.ToolInput)
		if subagentTools[resolvedTool] {
			event.Type = hooks.SubagentCompleted
			event.ToolName = "Agent"
			event.ToolUseID = syntheticToolUseID(payload.SessionID, event.Timestamp, resolvedTool, payload.ToolInput)
		} else if stateAlteringTools[resolvedTool] {
			event.Type = hooks.ToolStepCompleted
			event.ToolName = normalizeGeminiToolName(resolvedTool)
			event.ToolUseID = syntheticToolUseID(payload.SessionID, event.Timestamp, resolvedTool, payload.ToolInput)
		} else {
			return nil, nil
		}
	default:
		return nil, nil
	}

	return event, nil
}

func normalizeGeminiToolName(name string) string {
	switch name {
	case "write_file", "save_file":
		return "Write"
	case "edit_file", "replace":
		return "Edit"
	case "run_shell_command":
		return "Bash"
	default:
		return name
	}
}

func syntheticToolUseID(sessionID string, ts int64, toolName string, toolInput json.RawMessage) string {
	hh := sha256.New()
	hh.Write([]byte(sessionID))
	_, _ = fmt.Fprintf(hh, ":%d:%s:", ts, toolName)
	hh.Write(toolInput)
	return "gemini-step-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

// DeriveProviderSessionID returns the DB session key for a transcript.
// JSONL transcripts carry a header sessionId; older transcripts fall
// back to the filename stem.
func (p *Provider) DeriveProviderSessionID(transcriptRef string) string {
	data, err := os.ReadFile(transcriptRef)
	if err != nil {
		return agentgemini.ExtractSessionID(transcriptRef)
	}
	t, err := agentgemini.ParseTranscript(data)
	if err != nil {
		return agentgemini.ExtractSessionID(transcriptRef)
	}
	return agentgemini.SessionIDFromTranscript(t, transcriptRef)
}

func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	data, err := os.ReadFile(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	t, err := agentgemini.ParseTranscript(data)
	if err != nil {
		return 0, nil
	}
	return len(t.Messages), nil
}

func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	data, err := os.ReadFile(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}

	t, err := agentgemini.ParseTranscript(data)
	if err != nil {
		return nil, offset, nil
	}

	providerSessionID := agentgemini.SessionIDFromTranscript(t, transcriptRef)

	model := hooks.ModelFromContext(ctx)

	// Derive SourceProjectPath from CWD saved in capture state.
	// Gemini transcripts don't embed CWD, so we rely on the hook payload.
	sourceProjectPath := hooks.CWDFromContext(ctx)

	meta := map[string]any{"source_key": transcriptRef}
	metaJSON, _ := json.Marshal(meta)

	var events []broker.RawEvent
	eventTs := time.Now().UnixMilli()

	for i := offset; i < len(t.Messages); i++ {
		msg := t.Messages[i]

		var role, kind string
		switch msg.Type {
		case "user":
			role = "user"
			kind = "user"
		case "gemini":
			role = "assistant"
			kind = "assistant"
		default:
			continue
		}

		// Extract model from transcript messages if not from hook context.
		if model == "" && msg.Model != "" {
			model = msg.Model
		}

		summary := agentgemini.Truncate(msg.Content, 200)
		tus := agentgemini.ExtractToolUses(msg)
		// Gemini may store tool-call paths relative to the session CWD.
		// Resolve before serialization so replay routes like direct hooks.
		for j := range tus {
			tus[j].FilePath = resolveGeminiFilePath(sourceProjectPath, tus[j].FilePath)
		}

		var contentTypes []string
		if msg.Content != "" {
			contentTypes = append(contentTypes, "text")
		}
		if len(tus) > 0 {
			contentTypes = append(contentTypes, "tool_use")
		}

		var tokensIn, tokensOut, tokensCacheRead int64
		if msg.Tokens != nil {
			tokensIn = msg.Tokens.Input
			tokensOut = msg.Tokens.Output
			tokensCacheRead = msg.Tokens.Cached
		}

		// Stable event ID.
		msgJSON, _ := json.Marshal(msg)
		hh := sha256.New()
		hh.Write([]byte(transcriptRef))
		_, _ = fmt.Fprintf(hh, ":%d:", i)
		hh.Write(msgJSON)
		eventID := hex.EncodeToString(hh.Sum(nil))

		var payloadHash string
		if bs != nil {
			payloadHash, _, _ = bs.Put(ctx, msgJSON)
		}

		toolUsesJSON := ""
		if s := agentgemini.SerializeToolUses(tus, contentTypes); s.Valid {
			toolUsesJSON = s.String
		}

		filePaths := broker.ExtractFilePaths(toolUsesJSON)

		events = append(events, broker.RawEvent{
			EventID:           eventID,
			SourceKey:         transcriptRef,
			Provider:          agentgemini.ProviderName,
			SourcePosition:    int64(i),
			Timestamp:         eventTs,
			Kind:              kind,
			Role:              role,
			ToolUsesJSON:      toolUsesJSON,
			Summary:           summary,
			PayloadHash:       payloadHash,
			TokensIn:          tokensIn,
			TokensOut:         tokensOut,
			TokensCacheRead:   tokensCacheRead,
			ProviderEventID:   msg.ID,
			FilePaths:         filePaths,
			ProviderSessionID: providerSessionID,
			SessionStartedAt:  eventTs,
			SessionMetaJSON:   string(metaJSON),
			SourceProjectPath: sourceProjectPath,
			Model:             model,
			EventSource:       "transcript",
		})
		eventTs++
	}

	// Backfill model onto all events if discovered mid-scan.
	if model != "" {
		for i := range events {
			if events[i].Model == "" {
				events[i].Model = model
			}
		}
	}

	return events, len(t.Messages), nil
}
