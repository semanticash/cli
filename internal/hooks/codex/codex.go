// Package codex provides hook-based capture for OpenAI Codex sessions.
//
// One provider covers both distributions of the product: the standalone
// `codex` CLI (Homebrew, npm) and the Codex desktop app, which embeds the
// same runtime over stdio JSON-RPC. Both honor user-global hook
// configuration at ~/.codex/hooks.json and the `[features] hooks = true`
// flag in ~/.codex/config.toml.
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

// New returns the Codex hook provider for explicit registration via
// providers.NewHookRegistry().
func New() *Provider { return &Provider{} }

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return displayName }

// IsAvailable reports whether Codex is present on this machine. The
// standalone CLI installs `codex` on PATH; the desktop app drops a
// bundle at /Applications/Codex.app on macOS plus a per-user state
// directory at ~/.codex. Either signal is enough to surface Codex as a
// known provider.
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

// ParseHookEvent translates Codex hook stdin into a normalized Event.
// Returns nil for hook names that have no capture analogue today so the
// dispatcher skips them quietly.
//
// Hook event mapping:
//   - session_start      -> SessionOpened (lifecycle no-op in the dispatcher)
//   - user_prompt_submit -> PromptSubmitted
//   - post_tool_use      -> ToolStepCompleted
//   - stop               -> AgentCompleted
//
// Codex does not currently emit a session-end signal. Capture state is
// removed at every AgentCompleted (turn end), matching how Claude Code's
// Stop is handled.
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

	// Codex sends its own provider turn id, but Semantica packages
	// provenance by the capture-state turn created for the user prompt.
	// Leave Event.TurnID empty so lifecycle can attach the active turn
	// before direct emission; otherwise tool steps would be stored under
	// a provider turn that is never packaged.
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

	// Hook names arrive in the kebab-case form the installer registers
	// (`semantica capture codex <name>`). Map each to the dispatcher's
	// event taxonomy.
	switch hookName {
	case "session-start":
		event.Type = hooks.SessionOpened
	case "user-prompt-submit":
		event.Type = hooks.PromptSubmitted
	case "post-tool-use":
		// Filter to the tools the direct emitter knows how to shape.
		// PostToolUse fires for every tool name our matcher regex
		// accepts, but only the four below carry data the scorer can
		// turn into attribution evidence: apply_patch and Bash from
		// the standalone CLI and desktop runtime, plus Write/Edit
		// from any future Codex release that emits them in the
		// Claude tool_input shape.
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

// codexHookPayload is the union of fields Codex's six hook events can
// send. Decoding via a single struct keeps the per-event branches in
// ParseHookEvent thin; unknown fields are ignored.
type codexHookPayload struct {
	SessionID string `json:"session_id"`
	// TurnID is Codex's provider turn id. Keep it decoded for future
	// use, but do not map it to hooks.Event.TurnID.
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

// isCapturableTool reports whether a tool fired through PostToolUse
// carries data the attribution pipeline can attribute. Codex's
// PostToolUse matcher is best-effort; the runtime may invoke us for
// tools that match the regex syntactically but contribute no
// per-line evidence.
func isCapturableTool(name string) bool {
	switch name {
	case "apply_patch", "Bash", "Write", "Edit":
		return true
	}
	return false
}

// TranscriptOffset returns 0 unconditionally. Codex's rollout files are
// not used for capture today; hooks supply every event we need.
func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	return 0, nil
}

// ReadFromOffset is a no-op. Codex rollout files (~/.codex/sessions/...)
// are not a stable contract and have observed non-strict JSONL where
// record strings embed raw newlines. The provider relies exclusively on
// hook payloads. If a fallback read path becomes useful, it should use
// a streaming JSON decoder rather than line-based splitting.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	return nil, offset, nil
}

// payloadCwd is the subset of every Codex hook payload the cwd preflight
// reads. Other fields are ignored at this stage; full parsing happens in
// ParseHookEvent once the capture pipeline lands.
type payloadCwd struct {
	Cwd string `json:"cwd"`
}

// peekCwd extracts the session's working directory from a raw hook
// payload without consuming surrounding state. Returns the empty string
// when the payload is missing the field or cannot be parsed; the
// preflight treats either case as "do not capture".
func peekCwd(raw []byte) string {
	var p payloadCwd
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Cwd
}
