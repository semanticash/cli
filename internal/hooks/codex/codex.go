// Package codex provides hook-based capture for OpenAI Codex sessions.
//
// Hooks are configured in <repo>/.codex/hooks.json. Codex surfaces that do
// not emit project hooks are outside this provider's capture path.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
)

const (
	providerName = "codex"
	displayName  = "OpenAI Codex"
)

// Provider implements hooks.HookProvider for OpenAI Codex.
type Provider struct{}

// New returns a Codex hook provider.
func New() *Provider { return &Provider{} }

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return displayName }

// IsAvailable reports whether Codex appears to be installed.
func (p *Provider) IsAvailable() bool {
	if util.ResolveExecutable([]string{"codex"}) != "" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); err == nil {
		return true
	}
	if _, err := os.Stat("/Applications/Codex.app"); err == nil {
		return true
	}
	return false
}

// ParseHookEvent converts Codex hook input to a normalized event.
//
// Hook event mapping:
//   - session_start      -> SessionOpened (lifecycle no-op in the dispatcher)
//   - user_prompt_submit -> PromptSubmitted
//   - pre_tool_use       -> ToolStepStarted (Bash only; pre-execution snapshot)
//   - post_tool_use      -> ToolStepCompleted
//   - stop               -> AgentCompleted
//
// Codex Stop ends a turn, not the session.
func (p *Provider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*hooks.Event, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read codex hook stdin: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var payload codexHookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse codex hook payload: %w", err)
	}

	// Lifecycle assigns the active Semantica turn.
	event := &hooks.Event{
		SessionID:     payload.SessionID,
		TranscriptRef: payload.TranscriptPath,
		Prompt:        payload.Prompt,
		Model:         payload.Model,
		Timestamp:     time.Now().UnixMilli(),
		CWD:           payload.CWD,
		ToolName:      payload.ToolName,
		ToolInput:     payload.ToolInput,
		ToolResponse:  payload.ToolResponse,
		ToolUseID:     payload.ToolUseID,
	}

	switch hookName {
	case "session-start":
		event.Type = hooks.SessionOpened
	case "user-prompt-submit":
		event.Type = hooks.PromptSubmitted
	case "pre-tool-use":
		if payload.ToolName != "Bash" {
			return nil, nil
		}
		event.Type = hooks.ToolStepStarted
	case "post-tool-use":
		// Ignore tools without a supported evidence shape.
		if !isCapturableTool(payload.ToolName) {
			return nil, nil
		}
		event.Type = hooks.ToolStepCompleted
	case "stop":
		event.Type = hooks.AgentCompleted
	default:
		return nil, nil
	}
	return event, nil
}

// codexHookPayload contains the fields used across Codex hook events.
type codexHookPayload struct {
	SessionID string `json:"session_id"`
	// TurnID is retained for compatibility but not mapped to Event.TurnID.
	TurnID               string          `json:"turn_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	Model                string          `json:"model"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// isCapturableTool reports whether PostToolUse has a supported evidence shape.
func isCapturableTool(name string) bool {
	switch name {
	case "apply_patch", "Bash", "Write", "Edit":
		return true
	}
	return false
}

// TranscriptOffset returns zero because Codex capture does not replay files.
func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	return 0, nil
}

// ReadFromOffset is a no-op because this provider relies on hook payloads.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	return nil, offset, nil
}

// payloadCwd is the hook payload subset used by capture preflight.
type payloadCwd struct {
	Cwd string `json:"cwd"`
}

// peekCwd returns the hook working directory, or empty for invalid input.
func peekCwd(raw []byte) string {
	var p payloadCwd
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Cwd
}
