# Semantica CLI Reference

Full command reference for the Semantica CLI. For an overview and quick start, see [README.md](README.md).

---

## Global flags

| Flag | Description |
|------|-------------|
| `--repo <path>` | Path to the Git repository (default: current directory) |
| `--version` | Print version and exit (release builds include the short commit) |
| `--help` | Show help |

---

## Commands

### `semantica enable`

Initializes Semantica in the current repo. Creates the `.semantica/` directory, installs Git hooks, detects AI agents, and creates a baseline checkpoint.

```bash
semantica enable                             # First-time setup (interactive provider selection)
semantica enable --force                     # Re-detect providers and reinstall hooks
semantica enable --providers claude-code     # Non-interactive: specify providers directly
semantica enable --providers kiro-ide        # Install Kiro IDE hooks only
semantica enable --providers kiro-cli        # Install Kiro CLI hooks only
```

| Flag | Default | Description |
|------|---------|-------------|
| `--force` | `false` | Reinitialize Semantica state if already enabled |
| `--providers` | | Agents to install hooks for (e.g. `claude-code,cursor,kiro-ide,kiro-cli`) |
| `--json` | `false` | Output as JSON |

### `semantica disable`

Disables Semantica. Hooks remain installed but become inert. Your data in `.semantica/` is preserved.

```bash
semantica disable
semantica disable --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |

### `semantica status`

Shows an overview of AI activity in the repository - enabled state, authentication, workspace tier, repo connection state, endpoint, settings, last checkpoint, recent sessions, AI attribution trend, broker status, and update availability.

```bash
semantica status
semantica status --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |

### `semantica list`

Lists checkpoints for the repo, most recent first.

```bash
semantica list              # Last 20 checkpoints
semantica list -n 50        # Last 50
semantica list --json       # JSON output
semantica list --jsonl      # JSONL output (one object per line)
```

| Flag | Default | Description |
|------|---------|-------------|
| `-n, --limit` | `20` | Maximum number of checkpoints to list |
| `--json` | `false` | Output as JSON |
| `--jsonl` | `false` | Output as JSONL (one JSON object per line) |

### `semantica show <checkpoint_id>`

Shows details of a specific checkpoint - metadata, manifest hash, size, linked commit, and file list with blob hashes.

Checkpoint IDs are prefix-matchable - you only need enough characters to be unique.

```bash
semantica show abc123
semantica show abc123 --json
semantica show abc123 --jsonl     # metadata + one file per line
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |
| `--jsonl` | `false` | Output as JSONL (metadata + one file per line) |

### `semantica blame <ref>`

Shows AI attribution for a commit or checkpoint. Reports how much of the commit is AI-attributed, broken down by file.

```bash
semantica blame HEAD           # Latest commit
semantica blame abc1234        # By commit hash
semantica blame HEAD --json    # Full JSON with per-file detail
```

If no ref is given and stdin is a TTY, an interactive checkpoint picker is shown.

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON (includes per-file breakdown) |

### `semantica explain <commit>`

Explains what happened in a commit - files changed, AI involvement, session breakdown, and top edited files. Optionally generates an LLM playbook summary.

```bash
semantica explain HEAD                          # Show commit stats + AI involvement
semantica explain abc1234 --generate            # Also generate a playbook summary
semantica explain abc1234 --generate --force    # Regenerate even if summary exists
semantica explain abc1234 --json                # JSON output
```

The `--generate` flag spawns a background LLM call. Run `explain` again after a few seconds to see the result.

| Flag | Default | Description |
|------|---------|-------------|
| `--generate` | `false` | Generate a narrative explanation using an LLM |
| `--force` | `false` | Force regeneration of the playbook (use with `--generate`) |
| `--json` | `false` | Output as JSON |

### `semantica suggest commit`

Generates a concise commit message from all uncommitted changes (staged, unstaged, and untracked). Most suggestions are a single sentence, but broader diffs may use two short adjacent sentences on the same line. Analyzes the diff and recent AI session context using the first available LLM provider (Claude Code, Cursor CLI, Gemini CLI, or Copilot CLI). Copies the result to the clipboard automatically.

```bash
semantica suggest commit
semantica suggest commit --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |

### `semantica suggest pr`

Generates a PR title and body from the current branch diff against a base branch. If a pull request template exists, Semantica fills its sections instead of inventing a new structure. Warns when the working tree has uncommitted changes, since they are not included in the suggestion.

```bash
semantica suggest pr
semantica suggest pr --base origin/main
semantica suggest pr --json
semantica suggest pr --copy
```

| Flag | Default | Description |
|------|---------|-------------|
| `--base` | `auto-detect` | Base branch or ref to diff against |
| `--json` | `false` | Output as JSON |
| `--copy` | `false` | Copy the title and body to the clipboard |

### `semantica suggest implementations [implementation_id]`

Suggests titles, summaries, review priorities, and merge candidates for
implementations. Without an argument, Semantica analyzes implementation stories
across the current local graph and suggests titles for untitled items plus
possible merges. With an implementation ID, it generates a title, summary, and
review-priority view for that implementation.

```bash
semantica suggest implementations
semantica suggest implementations abc123
semantica suggest implementations abc123 --apply
semantica suggest implementations abc123 --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |
| `--apply` | `false` | Apply the suggested title in single-implementation mode |

### `semantica implementations [implementation_id]`

Lists cross-repo implementations, or shows the detail view for one
implementation. Alias: `semantica impl`.

Implementations are Semantica's concrete record for agent work that often feels
like a single story across repositories.

```bash
semantica implementations
semantica implementations --include-single
semantica implementations --all
semantica impl abc123
semantica impl abc123 --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Show all implementations, including old dormant and single-repo items |
| `--include-single` | `false` | Include single-repo implementations in the list |
| `--limit` | `20` | Maximum implementations to list |
| `--json` | `false` | Output as JSON |

#### `semantica implementations close <implementation_id>`

Closes an implementation. Closing is idempotent.

```bash
semantica implementations close abc123
```

#### `semantica implementations link <implementation_id>`

Manually links a session or commit to an implementation.

```bash
semantica implementations link abc123 --session sess_123
semantica implementations link abc123 --commit deadbeef
semantica implementations link abc123 --session sess_123 --repo /path/to/repo --force
```

| Flag | Default | Description |
|------|---------|-------------|
| `--session` | | Session ID to link |
| `--commit` | | Commit SHA to link |
| `--repo` | `current repo` | Repository path used for lookup |
| `--force` | `false` | Move a session from another implementation without confirmation |

#### `semantica implementations merge <target_id> <source_id>`

Merges the source implementation into the target implementation and closes the
source.

```bash
semantica implementations merge target123 source456
```

### `semantica tidy`

Performs safe housekeeping on transient Semantica state. It can prune stale
broker registry entries, remove abandoned capture state files, mark old
incomplete checkpoints as failed, remove orphan playbook FTS rows, and report
implementation cleanup opportunities such as stale dormant implementations and
unresolved conflicts. By default it runs in dry-run mode and reports what would
change.

```bash
semantica tidy
semantica tidy --apply
semantica tidy --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--apply` | `false` | Perform the reported changes |
| `--json` | `false` | Output as JSON |

### `semantica completion`

Generates shell completion scripts for Bash, Zsh, Fish, or PowerShell.

Homebrew installs completions automatically. For shell script or source installs, load them manually from the command output.

```bash
source <(semantica completion zsh)     # zsh
source <(semantica completion bash)    # bash
semantica completion fish | source     # fish
semantica completion powershell        # PowerShell script
```

### `semantica sessions`

Lists agent sessions tracked in the repo, or views a specific session's details.

```bash
semantica sessions                              # List recent sessions
semantica sessions --limit 100                  # More sessions
semantica sessions --all                        # Include sessions with no events
semantica sessions <session_id>                 # View session details (full ID or prefix)
semantica sessions <session_id> --transcript    # View session transcript (full ID or prefix)
```

| Flag | Default | Description |
|------|---------|-------------|
| `--limit` | `50` | Maximum number of sessions to list |
| `--all` | `false` | Include sessions with no events |
| `--transcript` | `false` | Show session transcript |
| `--json` | `false` | Output as JSON |

### `semantica transcripts [ref]`

Shows the agent transcript for a checkpoint or session - the sequence of user messages, assistant responses, and tool calls.

The ref argument is resolved as a checkpoint ID, commit hash, or session ID. Use `--checkpoint` or `--session` to force resolution mode.

```bash
semantica transcripts HEAD                  # Transcript for latest commit's checkpoint
semantica transcripts abc123                # By checkpoint or commit ref
semantica transcripts abc123 --commit       # Only sessions that touched files in the commit
semantica transcripts abc123 --by-session   # Group events by session
semantica transcripts abc123 --cumulative   # All events up to this checkpoint (not just delta)
semantica transcripts abc123 --raw          # Include full payload JSON from blob store
semantica transcripts abc123 --verbose      # Show provider, tokens, etc.
semantica transcripts abc123 --checkpoint   # Force resolution as checkpoint
semantica transcripts abc123 --session      # Force resolution as session
```

| Flag | Default | Description |
|------|---------|-------------|
| `--commit` | `false` | Show only sessions that touched files in the commit diff |
| `--by-session` | `false` | Group events by session |
| `--cumulative` | `false` | Show all events up to checkpoint (default: delta since previous) |
| `--raw` | `false` | Include raw payload JSON (loads from blob store) |
| `--verbose` | `false` | Show more fields (provider, tokens, etc.) |
| `--filter-session` | | Filter to a specific session ID (checkpoint mode only) |
| `--checkpoint` | `false` | Force resolution as checkpoint |
| `--session` | `false` | Force resolution as session |
| `--json` | `false` | Output as JSON |
| `--jsonl` | `false` | Output as JSONL (meta + one event per line) |

### `semantica checkpoint`

Manually creates a checkpoint (outside the normal commit flow).

```bash
semantica checkpoint -m "Before big refactor"
semantica checkpoint --json
```

| Flag | Default | Description |
|------|---------|-------------|
| `-m, --message` | | Checkpoint message |
| `--auto` | `false` | Auto checkpoint (used internally) |
| `--trigger` | | Trigger label |
| `--json` | `false` | Output as JSON |

### `semantica rewind <checkpoint_id>`

Restores the working tree to the state captured in a checkpoint. Creates a safety checkpoint first so you can undo the rewind.

```bash
semantica rewind abc123                  # Restore files to checkpoint state
semantica rewind abc123 --exact          # Also delete files not present in the checkpoint
semantica rewind abc123 --no-safety      # Skip safety checkpoint (dangerous)
semantica rewind abc123 --json           # JSON output
```

| Flag | Default | Description |
|------|---------|-------------|
| `--exact` | `false` | Delete files not present in the checkpoint |
| `--no-safety` | `false` | Skip creating a safety checkpoint before rewind |
| `--json` | `false` | Output as JSON |

### `semantica agents`

Manage AI agent hooks. Shows detected agents and allows toggling which ones have hooks installed.

In interactive mode (TTY), presents a multi-select picker. In non-interactive mode, prints a status table.

Aliases: `agent`

```bash
semantica agents              # Interactive: toggle agent hooks
semantica agents --json       # JSON output: detection and installation status
```

| Flag | Default | Description |
|------|---------|-------------|
| `--json` | `false` | Output as JSON |

### `semantica launcher`

Manage the optional OS-backed worker launcher.

```bash
semantica launcher enable
semantica launcher status
semantica launcher disable
```

The launcher is experimental and currently supports macOS (launchd), Linux
(systemd user instance), and Windows (Task Scheduler). It is separate from
`semantica enable`: the normal Git hook flow works without it. Use it when
agents or IDE-integrated tools may create commits on your behalf and you want
the post-commit work to run more reliably through an OS-managed path.

#### `semantica launcher enable`

Installs the launcher definition for the current platform and registers it
with the local OS daemon manager.

```bash
semantica launcher enable
```

#### `semantica launcher status`

Shows launcher state from three sources at once:

- `~/.semantica/settings.json`
- the launcher definition file on disk
- the OS daemon manager itself (`launchctl`, `systemctl --user`, or `schtasks`)

```bash
semantica launcher status
```

#### `semantica launcher disable`

Unregisters the launcher, removes its definition file, and clears the launcher
setting.

```bash
semantica launcher disable
```

### `semantica set`

View or update Semantica settings.

```bash
semantica set                              # Show current settings
semantica set auto-playbook enabled        # Enable auto-playbook generation
semantica set auto-playbook disabled       # Disable auto-playbook generation
semantica set trailers enabled             # Enable attribution and diagnostics trailers
semantica set trailers disabled            # Checkpoint-only commits
```

#### Subcommands

| Subcommand | Arguments | Description |
|------------|-----------|-------------|
| `auto-playbook` | `enabled\|disabled\|on\|off\|true\|false` | Enable or disable auto-playbook generation after each commit |
| `trailers` | `enabled\|disabled\|on\|off\|true\|false` | Enable or disable `Semantica-Attribution` and `Semantica-Diagnostics` trailers (`Semantica-Checkpoint` is always included) |

### `semantica auth`

Manage authentication for optional hosted features.

```bash
semantica auth login       # OAuth login (GitHub or GitLab)
semantica auth logout      # Revoke session and delete local credentials
semantica auth status      # Show authentication status
```

Login opens a browser for OAuth authorization and polls until complete. Tokens are stored in OS secure storage (macOS Keychain, Linux Secret Service) when available, with automatic refresh on expiry. On headless or CI environments without a keyring, credentials fall back to `~/.config/semantica/credentials.json` (respects `$XDG_CONFIG_HOME`) with `0600` permissions.

The `SEMANTICA_API_KEY` environment variable overrides stored credentials (useful for CI).

`semantica auth login` authenticates you globally. It does not connect any repos. Use `semantica connect` in each repo you want to sync.

### `semantica connect` / `semantica disconnect`

Connect or disconnect the current repo for optional hosted features.

```bash
semantica connect             # Connect this repo for hosted features
semantica disconnect          # Stop syncing attribution from this repo
```

`connect` verifies your account access and marks the current repo as connected in `.semantica/settings.json`. `disconnect` stops future sync attempts for that repo.
`connect` also tries to sync a small batch of already-packaged provenance for
that repo, plus historical commit attribution that Semantica already captured
locally. Remaining history continues to drain on later checkpoints. Local
capture, checkpoints, attribution, rewind, and playbook generation continue
to run without any hosted connection.

If the repo is already connected through a shared workspace, `semantica connect`
can prompt to request access instead of creating another hosted connection.
In non-interactive environments, it prints next steps instead of prompting.

### `semantica workspace requests`

List, approve, or reject pending access requests for workspaces you manage.

```bash
semantica workspace requests
semantica workspace requests approve <request-id>
semantica workspace requests reject <request-id>
```

## Settings

Settings live in `.semantica/settings.json`:

```json
{
  "enabled": true,
  "version": 1,
  "providers": ["claude-code", "cursor", "kiro-cli"],
  "connected": false,
  "connected_repo_id": "",
  "trailers": true,
  "automations": {
    "playbook": {
      "enabled": true
    }
  }
}
```

| Field | Description |
|-------|-------------|
| `enabled` | Master switch. `false` disables all hooks. |
| `version` | Schema version (currently `1`). |
| `providers` | List of providers with hooks installed. |
| `connected` | Whether this repo attempts hosted sync (set by `semantica connect`). |
| `connected_repo_id` | Repo-local connection metadata written when hosted sync is enabled. |
| `trailers` | Controls whether attribution and diagnostics trailers are appended. `Semantica-Checkpoint` is always included. Defaults to `true`. |
| `automations.playbook.enabled` | Auto-generate LLM playbook summaries on every commit. |

---

## File structure

```
.semantica/
  settings.json       # Configuration
  lineage.db          # SQLite database (checkpoints, sessions, events, attribution, playbooks)
  objects/            # Content-addressed blob store (file snapshots, manifests)
  worker.log          # Background worker + auto-playbook logs
```

Everything stays local by default. `.semantica/` is added to `.gitignore` automatically by `semantica enable`. Hosted sync only starts after `semantica connect` for the current repo.

Some cross-repo state also lives under Semantica's global home directory
(`$SEMANTICA_HOME`), including the broker registry and `implementations.db`.
When the launcher is enabled, `$SEMANTICA_HOME/worker-launcher.log`
records launcher-level events while per-repo job output continues to go to
`.semantica/worker.log`.
