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
	"io"
	"os"
	"path/filepath"

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

func init() {
	hooks.RegisterProvider(&Provider{})
}

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
	// Event parsing is intentionally inert until Codex payload capture is
	// enabled. Returning nil keeps installed hooks non-disruptive.
	_ = stdin
	_ = hookName
	return nil, nil
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
