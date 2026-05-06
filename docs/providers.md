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

**Hook config**: `.claude/settings.local.json`

Claude Code stores conversation transcripts as JSONL files in project-specific directories under `~/.claude/projects/`. Each line is a typed event (`system`, `human`, `assistant`, `result`).

### Hooks

Semantica registers the following hooks in `.claude/settings.local.json`:

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

The Cursor provider is detected by checking for Cursor's home-directory state
under `~/.cursor/`, which is shared across the supported desktop platforms.

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
Cursor after `semantica enable` so the new hooks are loaded. See the
[agent reload note](#reloading-agents-after-enable) below.

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

Detected by checking for Kiro's `globalStorage` directory:

- macOS: `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent/`
- Windows: `%APPDATA%/Kiro/User/globalStorage/kiro.kiroagent/`
- Linux: `~/.config/Kiro/User/globalStorage/kiro.kiroagent/`

### Hooks

Semantica installs repo-local Kiro hooks in `.kiro/hooks/` using `runCommand` actions:

- **`promptSubmit`** - Resolves and pins the session history reference for the current workspace so the matching stop hook can reuse it.
- **`fileEdited`** - Performs incremental mid-turn capture after file edits. The hook includes `patterns: ["**/*"]`, which Kiro requires before `fileEdited` can match.
- **`agentStop`** - Scans Kiro's execution trace store for the pinned session, extracts file operations, and routes them to the repo.

Unlike Claude, Cursor, Gemini, and Copilot, Kiro IDE does not expose an explicit session ID to external hook commands. Semantica uses a workspace-scoped capture-state key to reuse the session chosen at `promptSubmit` for both incremental `fileEdited` scans and the final `agentStop` sweep. Trace reads filter back to the pinned session and rely on deterministic event IDs for idempotent writes.

### Attribution

Kiro execution traces include structured file operations such as `create`, `replace`, `append`, and `smartRelocate`. Semantica maps create actions to canonical `Write` events and replace/append actions to canonical `Edit` events when the trace includes old and new content, enabling line-level attribution. `smartRelocate` remains file-touch attribution because Kiro does not provide source content for the rename.

### Limitations

- Kiro IDE hook commands do not receive an explicit session ID, so session selection at prompt submission is still best-effort when multiple Kiro chats exist for the same workspace.
- `smartRelocate` rename actions are attributed at file-touch granularity because there is no old/new content payload to score line by line.

---

## Kiro CLI

**Hook config**: `.kiro/agents/semantica.json`

Kiro CLI stores parent conversation history in a SQLite database, writes AgentCrew child sessions as JSONL files, and exposes hook payloads as JSON on stdin. Semantica uses direct hooks as the primary capture path for Kiro CLI 2.2+: prompt, file-edit, shell, subagent, and session-boundary events are emitted from hook payloads as they arrive. Conversation lookup is best-effort and is not required for direct file or shell attribution.

### Detection

Detected by resolving one of the following executables via `PATH`, then common user and package manager install locations (`~/.local/bin`, `/opt/homebrew/bin`, npm/pnpm/bun prefix directories, etc.):

- `kiro-cli`
- `kiro`

### Hooks

Semantica installs a dedicated repo-local Kiro CLI agent profile at `.kiro/agents/semantica.json` with seven hooks:

- **`agentSpawn`** - Opens a workspace-scoped capture session.
- **`userPromptSubmit`** - Saves prompt and workspace capture state. Conversation lookup is best-effort.
- **`preToolUse`** with matcher `subagent` - Captures the AgentCrew dispatch boundary.
- **`postToolUse`** with matcher `fs_write` - Captures Kiro `write` payloads for create, replace, and insert operations.
- **`postToolUse`** with matcher `execute_bash` - Captures Kiro `shell` payloads.
- **`postToolUse`** with matcher `subagent` - Captures the AgentCrew completion boundary.
- **`stop`** - Closes the workspace-scoped session, discovers child AgentCrew JSONL sessions, and flushes any pending lifecycle work.

Kiro CLI hook payloads include `cwd` and `prompt`, but they do not give Semantica an explicit parent conversation ID. Semantica pairs `userPromptSubmit` and `stop` through a workspace-scoped capture-state key and resolves the active conversation from the current workspace when it is useful. AgentCrew child sessions are discovered from Kiro's session directory with cwd, time-window, and session-shape guards.

### Attribution

Kiro CLI 2.2 file operations arrive as `write` payloads. Semantica maps `create` to `Write`, maps `strReplace` and `insert` to `Edit`, resolves relative paths against the hook working directory, and stores canonical file-edit content for line-level attribution. Shell operations arrive as `shell` payloads and are captured as `Bash` events.

AgentCrew subagent calls are captured at the parent boundary and, when discovery is unambiguous, Semantica replays each child JSONL session to attribute inner `write` and `shell` operations back to the parent turn. Child replay uses Kiro's own `toolUseId` values and links child provider sessions to the parent session for drill-down.

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
- Kiro CLI hooks do not expose a conversation ID directly, so conversation lookup is best-effort when multiple Kiro CLI chats exist for the same workspace.
- Direct `postToolUse` hooks own parent file and shell capture. Parent SQLite transcript replay stays disabled to avoid duplicate events with mismatched provider tool IDs.
- AgentCrew child discovery requires exactly one parent-shaped Kiro session in the same cwd and prompt-to-stop window. If the parent anchor is missing or multiple same-repo parents overlap, Semantica skips child replay rather than attaching children to the wrong parent.
- If `userPromptSubmit` is missed, later tool hooks may not have capture state to attach to for that turn.

---

## Gemini CLI

**Hook config**: `~/.gemini/settings.json`

Gemini CLI stores conversation history in project-specific directories under
`~/.gemini/tmp/`. Semantica supports both legacy JSON transcripts and newer
JSONL transcripts with a header session ID.

### Detection

Detected by checking for the existence of `~/.gemini/tmp/`. The project hash is computed from the repository's absolute path.

### Hooks

Semantica registers hooks in `~/.gemini/settings.json` following the same
lifecycle pattern as the other providers. File-edit and shell tool hooks are
captured directly when Gemini emits them, and transcript replay fills in session
metadata such as model, tokens, and the provider session ID.

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
- **`postToolUse`** - Captures direct file and shell provenance for `create`, `edit`, and `bash`.
- **`agentStop`** - Replays the transcript from the saved offset and packages the completed turn.
- **`sessionStart`** / **`sessionEnd`** - Lifecycle tracking and final flush.
- **`subagentStop`** - Captures the canonical subagent completion boundary.

Copilot CLI uses direct hook provenance for:

- **`create`** -> `Write`
- **`edit`** -> `Edit`
- **`bash`** -> `Bash`
- **`task`** -> `Agent`

The `task` tool provides the delegated-work prompt at dispatch time. Copilot reports completion through `subagentStop`; transcript replay remains in place as a fallback and as an additional source of session context.

### Limitations

- Copilot runs sub-tasks in-process. Subagent inner tool calls (file reads, edits, shell commands made inside a `task`-delegated agent) are not persisted by Copilot anywhere readable, so attribution captures only the dispatch prompt and the completion boundary. Direct user-driven edits get full line-level attribution.
- `subagentStop` is the canonical subagent completion boundary. The `task` post-tool-use hook is a dispatch acknowledgement and does not emit a separate completion event.
- `toolArgs` can arrive either as a JSON string or as a structured object, so the provider normalizes both shapes before capture.

---

## Provider detection

When you run `semantica enable`, the CLI calls each provider's `IsAvailable()` method. Detection varies by provider: some check for an executable on `PATH` and common install locations (Claude Code, Gemini CLI, Kiro CLI), some check for a provider-specific data directory (Cursor checks `~/.cursor`, Copilot checks `~/.copilot`), and some check for provider-managed global storage (Kiro IDE). For Claude Code, the CLI also discovers the native binary bundled inside the VS Code extension (`~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude`) when the standalone CLI is not on PATH. Detected providers are recorded as a string array in `.semantica/settings.json`:

```json
{
  "providers": ["claude-code", "cursor", "kiro-ide", "kiro-cli", "gemini", "copilot"]
}
```

Re-run `semantica enable --force` to re-detect providers after installing a new AI tool. Use `semantica agents` to interactively toggle which providers have hooks installed.

## Adding provider support

## Reloading agents after enable

When `semantica enable` installs hooks, agents that are already running may not
pick up the new configuration immediately. Most agents read their hook config at
session or workspace start, not continuously.

If an agent is already running when you enable Semantica, restart or reload/resume it:

| Provider | How to reload |
|----------|---------------|
| Claude Code | Type `/reload-plugins` in the active session, or start a new session |
| Cursor IDE | Reload the window (Ctrl+Shift+P > Reload Window) or restart Cursor |
| Gemini CLI | Start a new session |
| GitHub Copilot | Restart the IDE or CLI session |
| Kiro IDE | Reload the workspace or restart Kiro |
| Kiro CLI | Start a new session |

Until the agent reloads, Semantica cannot capture events from that session and
commits will show 0% AI attribution.

---

Provider integrations live in `internal/hooks/<provider>/`. Each provider implements the `HookProvider` interface: detection, hook install/uninstall, event parsing, transcript reading.

To add a new provider:

1. Create a package under `internal/hooks/<provider>/`
2. Implement `HookProvider` (see `internal/hooks/provider.go` for the interface)
3. Call `hooks.RegisterProvider()` in an `init()` function
4. Import the package in `internal/service/worker.go` (blank import for `init()` registration)
5. Add tests and documentation for the new provider
