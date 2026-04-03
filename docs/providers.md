# AI Provider Integrations

Semantica supports six AI coding providers. For each detected provider, Semantica installs repo-local hooks in the provider's configuration file. These hooks fire during agent activity and route captured events to the repo's lineage database via the broker.

Semantica reads session transcripts passively - it never modifies agent session logs or transcript files.

## Capture model

All providers share the same downstream packaging model:

1. **Prompt boundary** - A prompt hook saves provider-specific capture state so the matching completion hook can identify the same session, transcript, or workspace boundary.
2. **Direct hook events** (when available) - Some providers expose structured tool hooks for file edits, shell commands, or subagent boundaries. Semantica stores those events immediately as direct provenance.
3. **Transcript or trace replay** - Completion hooks read new provider data from the provider's transcript, trace store, or database and capture anything that was not available directly from the hook payload.
4. **Turn packaging** - Semantica packages a per-turn provenance bundle from the captured prompt and direct step events.

The exact storage and offset model is provider-specific. Some providers read from transcript offsets, some use provider-managed markers, and Kiro IDE scans execution traces at stop time. The background worker runs a reconciliation pass (`reconcileActiveSessions`) to flush any sessions that still have pending capture state, but the worker is not the main capture mechanism.

---

## Claude Code

**Hook config**: `.claude/settings.json`

Claude Code stores conversation transcripts as JSONL files in project-specific directories under `~/.claude/projects/`. Each line is a typed event (`system`, `human`, `assistant`, `result`).

### Hooks

Semantica registers the following hooks in `.claude/settings.json`:

- **`UserPromptSubmit`** - Saves the current transcript offset and records the prompt.
- **`PostToolUse[Write]`**, **`PostToolUse[Edit]`**, **`PostToolUse[Bash]`** - Capture direct file and shell provenance from hook payloads.
- **`PreToolUse[Agent]`** - Captures the delegated subagent prompt.
- **`PostToolUse[Agent]`** - Captures the delegated subagent boundary.
- **`Stop`** - Replays the transcript from the saved offset and packages the completed turn.
- **`SessionStart`** / **`SessionEnd`** - Lifecycle tracking and final flush.

Claude Code is currently the richest provider integration. Direct step events are captured from hook payloads, while transcript replay fills in session flow, token usage, and any events that were not emitted directly.

### Attribution

Claude Code tool calls include file paths and content. Semantica uses direct `Write` and `Edit` hook payloads plus transcript replay to build AI-generated code hashes (`ai_code_hashes`). During attribution, each changed line in a commit is compared against these hashes to determine AI authorship.

---

## Cursor

**Hook config**: `.cursor/hooks.json`

The Cursor provider covers both Cursor IDE and Cursor CLI. Both share the same `.cursor/` configuration directory and hooks file, but the hook surface is not identical across the two environments.

Cursor stores interaction data in multiple formats depending on the version:

1. **Legacy `.vscdb`** - SQLite databases in Cursor's workspace storage directory containing conversation threads and AI completions.
2. **Modern `ai-code-tracking.db`** - A dedicated SQLite database for tracking AI-generated code regions.
3. **Agent JSONL** - Transcript files from Cursor's agent/composer mode, stored as JSONL with tool calls and responses.

Semantica uses hook payloads, transcript replay, and Cursor's local tracking stores during ingestion.

### Detection

The Cursor provider is detected by searching for Cursor's application data directory:

- macOS: `~/Library/Application Support/Cursor/User/`
- Linux: `~/.config/Cursor/User/`

### Hooks

For Cursor IDE, Semantica registers hooks in `.cursor/hooks.json` for:

- **`beforeSubmitPrompt`** - Saves the current transcript boundary and records the prompt.
- **`preToolUse`** - Captures subagent prompt boundaries.
- **`postToolUse`** - Captures shell provenance.
- **`afterFileEdit`** - Captures direct file edit and file write provenance.
- **`stop`** - Replays the transcript from the saved offset and packages the completed turn.
- **`subagentStop`** - Captures the parent `Agent` step and triggers child transcript discovery.
- **`sessionStart`** / **`sessionEnd`** / **`preCompact`** - Lifecycle tracking, final flush, and offset reset handling.

Cursor IDE is now handled with the same direct-provenance packaging model as Claude for prompt, file edit, shell, and subagent boundary events.

Cursor CLI still shares the same configuration file, but its hook surface is currently more limited. Semantica treats it as transcript-first today and does not assume full parity with the IDE for direct file-step capture.

If Cursor IDE is already running when you enable Semantica, it may not pick up
changes to `.cursor/hooks.json` immediately. Reload the Cursor window or restart
Cursor after `semantica enable` so the new hooks are loaded.

### Limitations

- Cursor's internal database format is not a public API and may change between versions.
- The legacy `.vscdb` format contains many workspace state entries beyond AI interactions - Semantica filters for relevant keys.
- Cursor IDE subagent child sessions are discovered and linked, but child-side file mutations may still be text-only when Cursor does not expose structured file hooks for the child conversation.
- Cursor CLI support is still more limited than Cursor IDE for direct hook provenance.

---

## Kiro IDE

**Hook config**: `.kiro/hooks/*.kiro.hook`

Kiro IDE stores per-workspace session indexes and per-session history files under its application data directory. Semantica reads the workspace session index plus execution traces to capture file operations and route them to the repo.

### Detection

Detected by checking for Kiro's globalStorage directory:

- macOS: `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent/`
- Linux: `~/.config/Kiro/User/globalStorage/kiro.kiroagent/`

### Hooks

Semantica installs repo-local Kiro hooks in `.kiro/hooks/` using `runCommand` actions:

- **`promptSubmit`** - Resolves and pins the session history reference for the current workspace so the matching stop hook can reuse it.
- **`agentStop`** - Scans Kiro's execution trace store for the pinned session, extracts file operations, and routes them to the repo.

Unlike Claude, Cursor, Gemini, and Copilot, Kiro IDE does not expose an explicit session ID to external hook commands. Semantica pairs `promptSubmit` and `agentStop` through a workspace-scoped capture-state key and pins the session chosen at prompt submission. At stop time it scans the execution trace store, filters traces back to that session, and relies on deterministic event IDs for idempotent writes.

### Attribution

Kiro execution traces include structured file operations such as `create`, `append`, and `smartRelocate`. In the current implementation, Semantica uses these as provider file-edit signals, which gives file-level attribution. Line-level exact matching from Kiro content blobs is reserved for a later iteration.

### Limitations

- Kiro IDE hook commands do not receive an explicit session ID, so session selection at prompt submission is still best-effort when multiple Kiro chats exist for the same workspace.
- Kiro attribution is currently file-level rather than exact line-level.

---

## Kiro CLI

**Hook config**: `.kiro/agents/semantica.json`

Kiro CLI stores conversation history in a SQLite database and exposes hook payloads as JSON on stdin. Semantica reads the current workspace conversation from the Kiro CLI database, tracks the last processed file-writing tool call in provider-managed sidecar state, and routes new file-writing tool calls to the repo.

### Detection

Detected by resolving one of the following executables via `PATH`, then common user and package manager install locations (`~/.local/bin`, `/opt/homebrew/bin`, npm/pnpm/bun prefix directories, etc.):

- `kiro-cli`
- `kiro`

### Hooks

Semantica installs a dedicated repo-local Kiro CLI agent profile at `.kiro/agents/semantica.json` with two hooks:

- **`userPromptSubmit`** - Saves the current workspace conversation reference and records the current `fs_write` boundary for that workspace.
- **`stop`** - Reuses the pinned conversation when capture state exists, reads `fs_write` calls after the saved boundary from the Kiro CLI database, and routes them to the repo.

Kiro CLI hook payloads include `cwd` and `prompt`, but they do not give Semantica an explicit conversation ID. Semantica pairs `userPromptSubmit` and `stop` through a workspace-scoped capture-state key and resolves the active conversation from the current workspace.

### Attribution

Kiro CLI currently captures `fs_write` tool calls and turns them into provider file-edit signals. That gives file-level attribution today. Exact line-level matching from Kiro CLI tool content can be added in a later iteration.

### Usage

Kiro CLI stores behavior in named agent configs. Semantica installs a repo-local config named `semantica` at `.kiro/agents/semantica.json`.

If the current Kiro CLI session uses that config, Semantica capture is active. You can select it explicitly:

```bash
kiro-cli chat --agent semantica
```

Or make it the default for the current repo so plain `kiro-cli chat` uses it automatically:

```bash
kiro-cli agent set-default semantica
```

### Limitations

- Kiro CLI support in `v1` is tied to the repo-local `semantica` agent config. If Kiro CLI is using some other agent config, Semantica hooks will not be active for that session.
- Kiro CLI hooks do not expose a conversation ID directly, so conversation selection is still best-effort when multiple Kiro CLI chats exist for the same workspace.
- If `userPromptSubmit` is missed, the following `stop` event cannot recover the missing offset boundary for that turn.

---

## Gemini CLI

**Hook config**: `~/.gemini/settings.json`

Gemini CLI stores conversation history as JSON files in project-specific directories under `~/.gemini/tmp/`. Each file represents a complete chat session.

### Detection

Detected by checking for the existence of `~/.gemini/tmp/`. The project hash is computed from the repository's absolute path.

### Hooks

Semantica registers hooks in `~/.gemini/settings.json` following the same lifecycle pattern as the other providers.

---

## GitHub Copilot

**Hook config**: `.github/hooks/semantica.json`

Copilot CLI stores session transcripts as JSONL event files at `~/.copilot/session-state/<sessionId>/events.jsonl`, alongside a `workspace.yaml` with project metadata.

### Detection

Detected by checking for the existence of `~/.copilot`.

### Hooks

Semantica installs the following hooks in `.github/hooks/semantica.json`:

- **`userPromptSubmitted`** - Saves the current transcript offset and records the prompt.
- **`preToolUse`** - Captures subagent prompt boundaries before `task` delegation.
- **`postToolUse`** - Captures direct file, shell, and task completion provenance.
- **`agentStop`** - Replays the transcript from the saved offset and packages the completed turn.
- **`sessionStart`** / **`sessionEnd`** - Lifecycle tracking and final flush.
- **`subagentStop`** - Optional extra subagent metadata when the CLI emits it.

Copilot CLI uses direct hook provenance for:

- **`create`** -> `Write`
- **`edit`** -> `Edit`
- **`bash`** -> `Bash`
- **`task`** -> `Agent`

The `task` tool is the main delegated-work signal in the current Copilot CLI surface. Transcript replay remains in place as a fallback and as an additional source of session context.

### Limitations

- Copilot delegated work is currently modeled from `task` hooks, not from a Claude-style child transcript model.
- `toolArgs` can arrive either as a JSON string or as a structured object, so the provider normalizes both shapes before capture.

---

## Provider detection

When you run `semantica enable`, the CLI calls each provider's `IsAvailable()` method. Detection varies by provider: some check for an executable on `PATH` and common install locations (Claude Code, Gemini CLI, Kiro CLI), some check for a provider-specific data directory (Cursor checks `~/.cursor`, Copilot checks `~/.copilot`), and some check for provider-managed global storage (Kiro IDE). Detected providers are recorded as a string array in `.semantica/settings.json`:

```json
{
  "providers": ["claude-code", "cursor", "kiro-ide", "kiro-cli", "gemini", "copilot"]
}
```

Re-run `semantica enable --force` to re-detect providers after installing a new AI tool. Use `semantica agents` to interactively toggle which providers have hooks installed.

## Adding provider support

Provider integrations live in `internal/hooks/<provider>/`. Each provider implements the `HookProvider` interface: detection, hook install/uninstall, event parsing, transcript reading.

To add a new provider:

1. Create a package under `internal/hooks/<provider>/`
2. Implement `HookProvider` (see `internal/hooks/provider.go` for the interface)
3. Call `hooks.RegisterProvider()` in an `init()` function
4. Import the package in `internal/service/worker.go` (blank import for `init()` registration)
5. Add tests and documentation for the new provider
