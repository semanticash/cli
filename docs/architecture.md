# Architecture

This document describes how Semantica works internally.

## Overview

Semantica is a single Go binary that operates as a CLI tool, a set of Git hook handlers, and a background worker. It adds an attribution and observability layer on top of Git without modifying Git's workflow.

```text
  AI agent activity                               Git commit (repo A)
  (Claude, Cursor, etc.)
         |
   provider hooks                          pre-commit   commit-msg   post-commit
   (prompt, step, stop)                         |            |            |
         |                                  create       append       link commit
         |                                  pending      trailers     spawn worker
  +--------------+                               \           |           /
  |   capture    |                                \          |          /
  |   command    |                                 +---------+---------+
  +------+-------+                                           |
         |                                             +-----v------+
         |                                             |   Worker   |
  +------v-------+                                     | detached / |
  |    broker    |                                     | launcher   |
  |   (routing)  |                                     +-----+------+
         |                                                   |
  +---+------+---+                                  +---------+---------+
      |      |                                      |         |         |
      |      |                                  reconcile   build     compute
      |      |                                  sessions   manifest attribution
      |      |                                      |         |         |
      |      +-------------> +--------------+ <-----+---------+---------+
      |                      | .semantica/  |
      |                      | repo A       |
      |                      | lineage.db   |
      |                      +--------------+
      |
      +--------------------> +--------------+
                             | .semantica/  |
                             | repo B       |
                             | lineage.db   |
                             +--------------+
```

There are two ingestion paths:

1. **Real-time capture** (primary) - Provider hooks fire `semantica capture` during agent activity, routing events through the broker into one or more enabled repos.
2. **Worker reconciliation** (secondary) - Before processing a repository queue, the worker attempts to replay pending capture state owned by that repository. Cross-repository and unowned state remains pending and is reported by `semantica doctor`.

The broker fans out by file ownership. A capture started from one enabled repo can still write events into another enabled repo when the touched files belong there.

## Capture

The `semantica capture` command is the primary ingestion path for AI agent activity. It is invoked by provider hooks (not by the user directly). Each provider registers hooks in its own configuration file (e.g., `.claude/settings.local.json`, `.cursor/hooks.json`) that call `semantica capture <provider> <hook-name>` with event metadata on stdin.

The capture command:

1. Parses the provider-specific stdin payload
2. On prompt-submit: saves the current transcript offset to `$SEMANTICA_HOME/capture/` so it knows where new content starts
3. On direct tool hooks: stores prompt, file edit, shell, and subagent boundary events immediately when the provider exposes them
4. On stop: reads the transcript or provider store from the saved offset, extracts events, and routes them through the broker to the correct repo's database
5. Packages prompt, step, and bundle provenance blobs for each completed turn

When a new prompt arrives before pending transcript data is replayed, Semantica preserves ordered turn boundaries. Providers with authoritative offsets recover ownership by transcript position; other providers leave ambiguous ownership unset. If a session switches transcripts while evidence is pending, Semantica preserves the old state as an orphan snapshot reported by `semantica doctor`.

See [providers.md](providers.md) for provider-specific hook details.

## Git hooks

Semantica installs three Git hooks via `semantica enable`. Each hook invokes the `semantica` binary as a subprocess.

### pre-commit

Records the staged tree and current `HEAD`, then writes a handoff file
(`.semantica/.pre-commit-checkpoint`) containing the lineage record ID,
timestamp, tree, and parent boundary. It also attempts to create the pending
checkpoint row in SQLite. The handoff is written first so a later hook can
recover from a temporary database failure.

Internal failures are logged and do not reject the commit.

### commit-msg

Reads the lineage record ID from the handoff. It computes best-effort
attribution with a bounded active-session flush, always appends the lineage
trailer when the handoff is available, and appends attribution and diagnostics
trailers when the `trailers` setting is enabled:

```
Semantica-Checkpoint: chk_abc123
Semantica-Attribution: 42% claude_code (18/43 lines)
Semantica-Diagnostics: 3 files, lines: 15 exact, 2 modified, 1 formatted
```

If no AI matches the commit, the attribution trailer becomes `0% AI detected (0/N lines)` and the diagnostics trailer explains whether no AI events existed in the lineage window or whether events existed but did not match the committed files.

Duplicate trailers are not appended. A missing handoff, such as after
`git commit --no-verify`, leaves attribution best-effort and does not create a
commit linkage.

### post-commit

Reads the handoff and verifies that its staged tree and recorded parent match
the new commit. It then atomically promotes the handoff to a durable receipt
under `.semantica/commit-receipts/` before opening SQLite. The receipt preserves
the lineage record ID, commit SHA, and original pre-commit timestamp.

The fast path creates or finds the pending checkpoint, links the commit, and
removes the receipt. If SQLite is unavailable, the receipt remains for the
worker to drain later. Receipts are processed in creation order so a newer
commit cannot overtake an older pending link.

By default, Semantica then spawns a detached background worker process:

```text
semantica worker run --repo <dir> --checkpoint <id> --commit <hash>
```

Users can optionally enable an OS-managed launcher path with
`semantica launcher enable`. In that mode, post-commit writes a repo-local job
marker and asks the platform backend (launchd on macOS, systemd user units on
Linux, or Task Scheduler on Windows) to run `semantica worker drain`, which
discovers and processes pending markers across active repositories. Launcher
backends also drain every 30 minutes so expired leases and scheduled retries
recover after a worker exits unexpectedly.

The launcher records the binary identity at registration time. If a later
commit runs through a replaced Semantica binary, dispatch re-registers the
launcher before asking the OS backend to drain work. Users can also run
`semantica launcher refresh` after upgrades or local installs to re-bind the
worker immediately.

## Worker

The worker completes the internal lineage record created by pre-commit. It can run in two
ways:

1. The default detached worker spawned directly by post-commit
2. The optional OS-managed launcher worker that drains pending markers

Both paths use the same `WorkerService.Run` pipeline for each record.

Workers serialize processing per repository with `.semantica/worker.lock`.
Commit-linked records are drained in repository sequence and claimed with a
durable lease. Transient failures use bounded exponential backoff. A terminally
failed queue head blocks later records until the cause is fixed and
`semantica worker retry <checkpoint-id>` resets it. `semantica doctor` reports
lock state, scheduled retries, and blocked queues.

Status derives audit readiness from checkpoint state, attribution completion,
turn-bundle coverage, and hosted push markers. Each component reports an
explicit state under a named local or hosted policy.

### Processing pipeline

1. **Session reconciliation** - Attempts to replay pending capture state owned by the repository. Sessions that route across repositories remain pending for a later unscoped capture; unowned and orphaned segments are reported by `semantica doctor`.

2. **File manifest** - Commit-linked checkpoints use the linked commit's tracked
   Git tree, including canonical blob bytes without worktree filters. Manual and
   baseline checkpoints snapshot tracked and untracked, non-ignored worktree
   files. Files and manifests are stored in the repository CAS. Commit manifests
   reuse blobs only when Git object identity is unchanged.

3. **Session linking and statistics** - Resolves the session and attribution
   windows, links sessions that contributed events, and records checkpoint
   aggregates such as session and changed-file counts.

4. **AI attribution** - Diffs the commit against its first parent and scores
   the current commit-linked event window. Historical lookback is limited to
   files added by the commit that appear in the previous lineage manifest and
   have zero current-window AI lines. Modified files never inherit historical
   evidence. The line-level match classes are:
   - **Exact**: line matches AI output character-for-character
   - **Formatted**: match after normalizing whitespace
   - **Modified**: line belongs to a diff hunk overlapping captured AI output,
     without an exact or normalized line match

   Computes per-file and aggregate AI percentage and stores it on the lineage record. The default v2 scorer aligns verified tool deltas with committed lines and preserves unaligned changes as file-level evidence. Provider-touch-only lines are carried as `ai_provider_only_lines` and excluded from the headline AI percentage. Per-file results include a primary display evidence class plus the full list of contributing evidence classes so exact line matches, provider-touch fallback, carry-forward, and deletion signals remain distinguishable.

5. **Lineage completion** - Marks the pending checkpoint complete only after
   required enrichment has written its manifest and local state.

6. **Sync** (optional) - After completion, drains packaged turn provenance,
   pushes commit attribution, and advances historical backfill when the
   repository is connected. Hosted failures are recorded for retry and do not
   reopen the completed local checkpoint.

7. **Auto-playbook** (optional) - If enabled, runs `semantica _auto-playbook`
   in the background to generate a structured summary (title, intent, outcome,
   learnings, friction, keywords) and stores it on the lineage record.

Steps 6 and 7 are post-completion side effects.

## Storage

### SQLite database (`lineage.db`)

Single-file database in `.semantica/`. Contains:

| Table | Purpose |
|-------|---------|
| `repositories` | Repo records keyed by root path |
| `checkpoints` | Internal lineage record metadata, repository sequence, retry state, and processing lease |
| `commit_links` | Maps commit hashes to lineage record IDs |
| `agent_sources` | Provider source metadata keyed by provider and source key |
| `agent_sessions` | AI agent sessions (provider, model, timestamps, parent linkage) |
| `agent_events` | Captured prompt, assistant, tool, and provenance events |
| `agent_event_evidence_links` | Links captured events to tool-delta evidence |
| `provenance_manifests` | Per-turn packaged transcript/bundle metadata and upload state |
| `session_checkpoints` | Links sessions to the lineage records they influenced |
| `checkpoint_stats` | Lineage aggregates and attribution/sync completion markers |
| `remote_attribution_backfills` | Progress and retry state for historical hosted attribution sync |

The schema is defined in `internal/store/sqlite/schema/` and queries in `internal/store/sqlite/queries/`. Both are processed by [sqlc](https://sqlc.dev) to generate type-safe Go code.

### Blob store (`objects/`)

Content-addressed storage using SHA-256 hashing and zstd compression. Directory layout uses 2-character sharding:

```
objects/
  aa/
    aabbccdd...  (compressed blob)
  bb/
    ...
```

The repository CAS stores lineage file blobs and manifests, routed event
payloads, transcript slices, prompts, final responses, tool deltas, and
packaged provenance bundles. A global CAS under `SEMANTICA_HOME` temporarily
holds provider-hook payloads before broker routing; referenced blobs are
propagated into each destination repository CAS.

Attribution reads event payloads and tool-delta blobs from the CAS. Provenance
packaging and sync read prompts, responses, step payloads, deltas, and bundles
from the same store. The CAS therefore remains part of the evidence pipeline
independently of lineage restore functionality.

### Tool snapshot store (`tool-snapshots.git`)

`internal/toolsnap` can represent a worktree as an ephemeral Git tree without
writing objects or refs to the user repository. It stores private objects and
refs in `.semantica/tool-snapshots.git` and reads committed objects through a
read-only alternate to the repository object database.

Capture and delta generation are bounded by path, byte, line, and diff-work
limits. Text changes produce deterministic, context-free hunks. Binary files,
gitlinks, and truncated text files retain file-level evidence. Unknown status
records, unsupported file types, incompatible object formats, and over-limit
worktrees fail closed. Git environment
variables that could redirect repository access are removed from snapshot
subprocesses. Store commands ignore inherited Git configuration, and store-local
remotes, partial-clone settings, and config includes are removed before object
access. Snapshot capture therefore never contacts a Git remote.

The store supports SHA-1 and SHA-256 repositories and gives each linked
worktree a distinct private ref namespace. Committed objects are read through
the repository's common object database. If repository maintenance removes an
alternate object, capture fails without fabricating evidence.

A repository-scoped registry serializes overlapping tool windows under an
OS-backed lock. Closing tree identities survive retries, and timeout tombstones
keep delayed captures from being treated as line evidence.

Overlapping windows share a group with an immutable join horizon. If a member
remains active past that horizon, Semantica seals the group, starts later
captures in a fresh group, and recovers completed members as partial evidence
without rereading the workspace.

Bounded maintenance defers during capture, removes stale unreferenced refs, and
prunes expired objects without operating on the user repository.

Claude Code, Codex, and Cursor shell hooks capture canonical deltas and link
them to their tool events. Recovery runs during worker drains and with
`semantica tidy --apply`. By default, v2 scoring verifies and aligns these
deltas against committed lines; partial or ambiguous evidence remains
file-level only.

### Settings (`settings.json`)

```json
{
  "enabled": true,
  "version": 1,
  "providers": ["claude-code", "codex", "cursor", "copilot", "gemini-cli", "kiro-cli", "kiro-ide"],
  "connected": false,
  "connected_repo_id": "",
  "trailers": true,
  "attribution_v2": true,
  "automations": {
    "playbook": { "enabled": false }
  }
}
```

The `providers` field lists installed hook providers. `connected` controls hosted sync, and `connected_repo_id` stores the repository binding. `trailers` controls the optional attribution and diagnostics trailers; `Semantica-Checkpoint` is always included. `attribution_v2` controls tool-delta scoring and defaults to `true`; set it to `false` to opt out. `SEMANTICA_ATTRIBUTION_V2` can override it with `1`, `true`, `0`, or `false`. Missing settings use the default; malformed or unreadable settings fail closed to v1 scoring.

### Global paths

| Purpose | Default path | Override |
| --- | --- | --- |
| Runtime state (broker registry, global objects, capture state) | `~/.semantica` | `SEMANTICA_HOME` |
| Launcher log (launcher mode) | `~/.semantica/worker-launcher.log` | `SEMANTICA_HOME` |
| LaunchAgent plist (macOS launcher mode) | `~/Library/LaunchAgents/sh.semantica.worker.plist` | none |
| systemd user unit (Linux launcher mode) | `~/.config/systemd/user/sh.semantica.worker.service` | `XDG_CONFIG_HOME` |
| systemd user timer (Linux launcher mode) | `~/.config/systemd/user/sh.semantica.worker.timer` | `XDG_CONFIG_HOME` |
| Task Scheduler XML import file (Windows launcher mode) | `~/.semantica/sh.semantica.worker.xml` | `SEMANTICA_HOME` |
| User config (auth fallback, release check cache) | `~/.config/semantica` | `XDG_CONFIG_HOME` |

Repo-local state still lives in `.semantica/` inside each enabled repository.

## Broker

The broker is a cross-repo event routing layer used by the `capture` command. It maintains a registry of enabled repositories at `repos.json` in the global Semantica directory, which defaults to `~/.semantica` and can be overridden via the `SEMANTICA_HOME` environment variable.

When an AI provider hook fires (e.g., Claude Code's `user-prompt-submit`), the capture command:

1. Reads the event payload from stdin
2. Looks up which registered repo(s) contain the affected files (deepest-match rule)
3. Routes the event to the matching repo database or databases

This allows Semantica to capture AI activity even when the provider's hook system doesn't know about the repo structure. In practice, a hook fired from one workspace can still route events into another Semantica-enabled repo if that repo owns the touched paths.

## Package structure

The following tree shows the primary package boundaries. It is representative,
not a list of every source file.

```
cmd/semantica/              CLI entrypoint (main.go)
corpus/v1/                  Versioned attribution evaluation corpus
internal/
  agents/                   Provider transcript models and extractors
    api/                    Shared provider extraction contracts
    claude/                 Claude Code transcript parsing
    copilot/                GitHub Copilot transcript parsing
    cursor/                 Cursor transcript and store parsing
    gemini/                 Gemini CLI transcript parsing
    kiro/                   Shared Kiro CLI and IDE extraction
  attribution/              Attribution domain logic
    annotations/            Evidence annotation detection and extraction
    carryforward/           Historical-lookback eligibility
    eval/                   Corpus evaluation runner
    events/                 Canonical candidates and tool-delta claims
    reporting/              Evidence classes, diagnostics, and aggregation
    scoring/                V1 and v2 line scorers and delta alignment
  auth/                     Credentials, device flow, and workspace identity
  broker/                   Cross-repository event ownership and routing
  commands/                 Cobra commands and terminal rendering
  doctor/                   Hook benchmark record format and collection
  explain/                  Local and hosted commit explanation assembly
  git/                      Repository, commit, and Git-hook operations
  health/                   Read-only doctor checks and rendering
  hooks/                    Provider hook installation and capture lifecycle
    builder/                Canonical event construction helpers
    claude/                 Claude Code hooks
    codex/                  Codex CLI hooks
    copilot/                GitHub Copilot CLI hooks
    cursor/                 Cursor IDE and Agent CLI hooks
    gemini/                 Gemini CLI hooks
    kirocli/                Kiro CLI hooks and replay
    kiroide/                Kiro IDE hooks
  launcher/                 Optional launchd, systemd, and Task Scheduler worker
  llm/                      Provider-neutral generation and writer adapters
  mcp/                      MCP server and tool definitions
  platform/                 Cross-platform locks, processes, paths, and files
  provenance/               Turn packaging, responses, redaction, and upload
  providers/                Installed-provider discovery and composition
  redact/                   Gitleaks-backed local redaction
  service/                  Git hooks, workers, attribution, lineage, and sync
    handoff/                Cross-agent handoff bundle assembly
  skills/                   Built-in skill installation and integrity checks
  store/
    blobs/                  SHA-256 and zstd content-addressed storage
    sqlite/                 SQLite access, migrations, queries, and sqlc output
  toolsnap/                 Isolated Git snapshots and bounded tool deltas
  util/                     Settings, paths, logging, and shared helpers
  version/                  Build version and commit injection
e2e/                        End-to-end tests
```
