package copilot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	agentcopilot "github.com/semanticash/cli/internal/agents/copilot"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

const providerName = "copilot"

// Provider implements hooks.HookProvider for GitHub Copilot CLI.
type Provider struct{}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "GitHub Copilot" }

func (p *Provider) IsAvailable() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); err == nil {
		return true
	}
	return false
}

const semanticaMarker = "semantica capture copilot"

// copilotHooksFile represents the .github/hooks/semantica.json structure.
type copilotHooksFile struct {
	Version int                         `json:"version"`
	Hooks   map[string][]copilotHookDef `json:"hooks"`
}

type copilotHookDef struct {
	Type string `json:"type"`
	Bash string `json:"bash"`
}

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	hooksPath := filepath.Join(repoRoot, ".github", "hooks", "semantica.json")

	var cfg copilotHooksFile
	data, err := os.ReadFile(hooksPath)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return 0, fmt.Errorf("parse existing %s: %w", hooksPath, err)
		}
	}
	if cfg.Hooks == nil {
		cfg.Hooks = make(map[string][]copilotHookDef)
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
		{"userPromptSubmitted", bin + " capture copilot user-prompt-submitted"},
		{"preToolUse", bin + " capture copilot pre-tool-use"},
		{"postToolUse", bin + " capture copilot post-tool-use"},
		{"agentStop", bin + " capture copilot agent-stop"},
		{"sessionStart", bin + " capture copilot session-start"},
		{"sessionEnd", bin + " capture copilot session-end"},
		{"subagentStop", bin + " capture copilot subagent-stop"},
	}

	count := 0
	for _, def := range hookDefs {
		existing := cfg.Hooks[def.hookPoint]
		found := false
		for _, h := range existing {
			if strings.Contains(h.Bash, semanticaMarker) {
				found = true
				break
			}
		}
		if !found {
			cfg.Hooks[def.hookPoint] = append(existing, copilotHookDef{
				Type: "command",
				Bash: def.command,
			})
		}
		count++
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal hooks config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir .github/hooks: %w", err)
	}
	if err := os.WriteFile(hooksPath, out, 0o644); err != nil {
		return 0, fmt.Errorf("write hooks config: %w", err)
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	hooksPath := filepath.Join(repoRoot, ".github", "hooks", "semantica.json")

	data, err := os.ReadFile(hooksPath)
	if err != nil {
		return nil
	}

	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	for hookPoint, defs := range cfg.Hooks {
		var kept []copilotHookDef
		for _, h := range defs {
			if !strings.Contains(h.Bash, semanticaMarker) {
				kept = append(kept, h)
			}
		}
		if len(kept) > 0 {
			cfg.Hooks[hookPoint] = kept
		} else {
			delete(cfg.Hooks, hookPoint)
		}
	}

	// If no hooks remain, remove the file entirely.
	if len(cfg.Hooks) == 0 {
		return os.Remove(hooksPath)
	}

	out, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(hooksPath, out, 0o644)
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".github", "hooks", "semantica.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), semanticaMarker)
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".github", "hooks", "semantica.json"))
	if err != nil {
		return "", err
	}
	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	for _, defs := range cfg.Hooks {
		for _, h := range defs {
			if strings.Contains(h.Bash, semanticaMarker) {
				parts := strings.Fields(h.Bash)
				if len(parts) > 0 {
					return parts[0], nil
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hook found")
}

type stdinPayload struct {
	SessionID      string          `json:"sessionId"`
	TranscriptPath string          `json:"transcriptPath"`
	Prompt         string          `json:"prompt,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	Timestamp      int64           `json:"timestamp,omitempty"`
	ToolName       string          `json:"toolName,omitempty"`
	ToolArgs       json.RawMessage `json:"toolArgs,omitempty"`
	ToolResult     json.RawMessage `json:"toolResult,omitempty"`
	ToolCalls      []stdinToolCall `json:"toolCalls,omitempty"`
}

type stdinToolCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// deriveTranscriptPath computes the transcript path from a session ID.
// Copilot CLI stores transcripts at ~/.copilot/session-state/<sessionId>/events.jsonl.
func deriveTranscriptPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot", "session-state", sessionID, "events.jsonl")
}

func (p stdinPayload) transcriptRef() string {
	if strings.TrimSpace(p.TranscriptPath) != "" {
		return strings.TrimSpace(p.TranscriptPath)
	}
	return deriveTranscriptPath(p.SessionID)
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
		SessionID: payload.SessionID,
		Prompt:    payload.Prompt,
		Timestamp: payload.Timestamp,
		CWD:       payload.CWD,
	}
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	switch hookName {
	case "user-prompt-submitted":
		event.Type = hooks.PromptSubmitted
		event.TranscriptRef = payload.transcriptRef()
	case "pre-tool-use":
		taskCall := findCopilotToolCall(payload.ToolCalls, "task")
		if taskCall == nil {
			return nil, nil
		}
		event.Type = hooks.SubagentPromptSubmitted
		event.TranscriptRef = payload.transcriptRef()
		event.ToolName = "Agent"
		event.ToolUseID = strings.TrimSpace(taskCall.ID)
		if event.ToolUseID == "" {
			event.ToolUseID = syntheticCopilotToolUseID(payload.SessionID, payload.Timestamp, taskCall.Name, taskCall.Args)
		}
		event.ToolInput = normalizeEmbeddedJSON(taskCall.Args)
	case "post-tool-use":
		toolName := normalizeCopilotToolName(payload.ToolName)
		if toolName == "" {
			return nil, nil
		}
		event.TranscriptRef = payload.transcriptRef()
		event.ToolName = toolName
		event.ToolUseID = syntheticCopilotToolUseID(payload.SessionID, payload.Timestamp, payload.ToolName, payload.ToolArgs)
		event.ToolInput = normalizeEmbeddedJSON(payload.ToolArgs)
		event.ToolResponse = normalizeEmbeddedJSON(payload.ToolResult)
		if toolName == "Agent" {
			event.Type = hooks.SubagentCompleted
		} else {
			event.Type = hooks.ToolStepCompleted
		}
	case "agent-stop":
		event.Type = hooks.AgentCompleted
		event.TranscriptRef = payload.transcriptRef()
	case "session-start":
		event.Type = hooks.SessionOpened
	case "session-end":
		event.Type = hooks.SessionClosed
		event.TranscriptRef = payload.transcriptRef()
	case "subagent-stop":
		event.Type = hooks.SubagentCompleted
		event.TranscriptRef = payload.transcriptRef()
		event.ToolName = "Agent"
		event.ToolUseID = syntheticCopilotToolUseID(payload.SessionID, payload.Timestamp, "subagent-stop", payload.ToolArgs)
		event.ToolInput = normalizeEmbeddedJSON(payload.ToolArgs)
		event.ToolResponse = normalizeEmbeddedJSON(payload.ToolResult)
	default:
		return nil, nil
	}

	return event, nil
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
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB max line
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}

func normalizeCopilotToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "create":
		return "Write"
	case "edit":
		return "Edit"
	case "bash":
		return "Bash"
	case "task":
		return "Agent"
	default:
		return ""
	}
}

func normalizeEmbeddedJSON(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}

	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return json.RawMessage(strings.TrimSpace(s))
		}
	}

	return raw
}

func syntheticCopilotToolUseID(sessionID string, ts int64, toolName string, toolArgs json.RawMessage) string {
	hh := sha256.New()
	hh.Write([]byte(sessionID))
	_, _ = fmt.Fprintf(hh, ":%d:%s:", ts, strings.TrimSpace(toolName))
	hh.Write(bytes.TrimSpace(normalizeEmbeddedJSON(toolArgs)))
	return "copilot-step-" + hex.EncodeToString(hh.Sum(nil))[:16]
}

func findCopilotToolCall(toolCalls []stdinToolCall, want string) *stdinToolCall {
	want = strings.TrimSpace(strings.ToLower(want))
	for i := range toolCalls {
		if strings.TrimSpace(strings.ToLower(toolCalls[i].Name)) == want {
			return &toolCalls[i]
		}
	}
	return nil
}

func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	f, err := os.Open(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("copilot: transcript not found (subagent sessionId may be parent)",
				"transcript", transcriptRef)
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	providerSessionID := agentcopilot.ExtractSessionID(transcriptRef)

	// Read SourceProjectPath from workspace.yaml alongside events.jsonl.
	var sourceProjectPath string
	wsPath := filepath.Join(filepath.Dir(transcriptRef), "workspace.yaml")
	if wsData, err := os.ReadFile(wsPath); err == nil {
		sourceProjectPath = agentcopilot.ExtractCWDFromWorkspace(wsData)
	}

	model := hooks.ModelFromContext(ctx)

	meta := map[string]any{"source_key": transcriptRef}
	if sourceProjectPath != "" {
		meta["project_path"] = sourceProjectPath
	}
	metaJSON, _ := json.Marshal(meta)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	lineNum := 0
	var events []broker.RawEvent
	var shutdownTokens *agentcopilot.SessionTokens
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

		// Extract model from transcript if the hook didn't provide one.
		if model == "" {
			if m := agentcopilot.ExtractModelFromLine(line); m != "" {
				model = m
			}
		}

		// Capture session.shutdown token aggregates (appears at end of transcript).
		if st := agentcopilot.ExtractSessionShutdownTokens(line); st != nil {
			shutdownTokens = st
		}

		pl := agentcopilot.ParseLine(line)
		if pl.Role == "" {
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
		if s := agentcopilot.SerializeToolUses(pl.ToolUses, pl.ContentTypes); s.Valid {
			toolUsesStr = s.String
		}

		var firstToolUseID, firstToolName string
		if len(pl.ToolUses) == 1 {
			firstToolUseID = pl.ToolUses[0].ToolUseID
			firstToolName = pl.ToolUses[0].Name
		}

		// File paths come from two sources:
		// 1. tool.execution_complete -> pl.FilePaths (from toolTelemetry, already absolute)
		// 2. assistant.message tool requests -> extracted via broker.ExtractFilePaths
		filePaths := pl.FilePaths
		if len(filePaths) == 0 {
			filePaths = broker.ExtractFilePaths(toolUsesStr)
		}

		events = append(events, broker.RawEvent{
			EventID:           eventID,
			SourceKey:         transcriptRef,
			Provider:          agentcopilot.ProviderName,
			SourcePosition:    int64(lineNum),
			Timestamp:         eventTs,
			Kind:              pl.Kind,
			Role:              pl.Role,
			ToolUsesJSON:      toolUsesStr,
			Summary:           pl.Summary,
			PayloadHash:       payloadHash,
			TokensIn:          pl.TokensIn,
			TokensOut:         pl.TokensOut,
			FilePaths:         filePaths,
			ToolUseID:         firstToolUseID,
			ToolName:          firstToolName,
			ProviderSessionID: providerSessionID,
			SessionStartedAt:  eventTs,
			SessionMetaJSON:   string(metaJSON),
			SourceProjectPath: sourceProjectPath,
			Model:             model,
		})

		eventTs++
		lineNum++
	}

	// Backfill model onto all events if it was discovered mid-scan.
	if model != "" {
		for i := range events {
			if events[i].Model == "" {
				events[i].Model = model
			}
		}
	}

	// Distribute session.shutdown inputTokens proportionally across
	// assistant events that already have outputTokens, so per-event
	// token records reflect the session's true input cost.
	if shutdownTokens != nil && shutdownTokens.InputTokens > 0 {
		var totalOut int64
		for i := range events {
			totalOut += events[i].TokensOut
		}
		if totalOut > 0 {
			for i := range events {
				if events[i].TokensOut > 0 {
					events[i].TokensIn = shutdownTokens.InputTokens * events[i].TokensOut / totalOut
				}
			}
		}
	}

	return events, lineNum, scanner.Err()
}
