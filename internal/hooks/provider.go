package hooks

import (
	"context"
	"io"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
)

// HookProvider is implemented by each agent provider for hook-based capture.
type HookProvider interface {
	// Name returns the provider identifier (e.g., "claude-code").
	Name() string

	// DisplayName returns a human-friendly label (e.g., "Claude Code").
	DisplayName() string

	// IsAvailable reports whether the provider can be discovered on this
	// machine, either via an executable or provider-specific local state.
	IsAvailable() bool

	// InstallHooks writes hook configuration to the provider's config file
	// (e.g., <repoRoot>/.claude/settings.local.json). Returns the number
	// of hooks installed.
	InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error)

	// UninstallHooks removes Semantica hooks from the provider's repo-local
	// config file.
	UninstallHooks(ctx context.Context, repoRoot string) error

	// AreHooksInstalled checks if Semantica hooks are configured in the
	// given repo.
	AreHooksInstalled(ctx context.Context, repoRoot string) bool

	// HookBinary returns the executable name or path configured in the
	// provider's hook settings for the given repo.
	// Returns only the binary token, not the full command line with arguments.
	// Used by health checks to verify the binary is actually reachable via
	// exec.LookPath.
	HookBinary(ctx context.Context, repoRoot string) (string, error)

	// ParseHookEvent parses stdin JSON into a normalized Event.
	ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*Event, error)

	// TranscriptOffset returns the current position in the transcript
	// (line count for JSONL, message count for JSON).
	// Called during PromptSubmitted to capture the offset before the agent acts.
	TranscriptOffset(ctx context.Context, transcriptRef string) (int, error)

	// ReadFromOffset reads the transcript starting at the given offset and
	// returns parsed RawEvents. This is the enrichment step - extracting
	// file paths, token usage, tool calls from the provider's format.
	ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error)
}

// SubagentDiscoverer is an optional interface for providers that support
// subagent (child) transcripts stored separately from the parent transcript.
// When implemented, the SubagentCompleted handler scans for child transcripts
// and reads each one with its own capture state, ensuring subagent edits are
// attributed correctly.
type SubagentDiscoverer interface {
	// DiscoverSubagentTranscripts returns paths to all subagent transcript
	// files associated with the given parent transcript. The parent transcript
	// path is used to derive the subagents directory (provider-specific).
	DiscoverSubagentTranscripts(ctx context.Context, parentTranscriptRef string) ([]string, error)

	// SubagentStateKey returns a stable key for the subagent's capture state
	// file, derived from the subagent transcript path. Must be unique per
	// subagent and safe for use as a filename component.
	SubagentStateKey(subagentTranscriptRef string) string
}

// TranscriptPreparer is an optional interface for providers whose transcripts
// may not be fully flushed to disk when the hook fires. If implemented,
// PrepareTranscript is called before every ReadFromOffset.
type TranscriptPreparer interface {
	// PrepareTranscript ensures the transcript file is complete and readable.
	// Called before ReadFromOffset. Must block until the file is ready or
	// a timeout is reached. Return nil on timeout so
	// capture proceeds with whatever data is available.
	PrepareTranscript(ctx context.Context, transcriptRef string) error
}

// DirectHookEmitter is an optional interface for providers that can emit
// RawEvents directly from hook payloads without waiting for transcript replay.
// Used by providers that expose structured hook payloads for prompt and
// tool events such as Write, Edit, Bash, and Agent.
type DirectHookEmitter interface {
	// BuildHookEvents constructs RawEvents from a hook Event's payload fields.
	// Called by the dispatcher for ToolStepCompleted, SubagentPromptSubmitted,
	// SubagentCompleted, and optionally PromptSubmitted. The provider stores
	// blobs via bs and returns fully populated RawEvents ready for routing and
	// writing.
	BuildHookEvents(ctx context.Context, event *Event, bs api.BlobPutter) ([]broker.RawEvent, error)
}
