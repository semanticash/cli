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
  |    broker    |                                     |  launchd   |
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
2. **Worker reconciliation** (secondary) - The background worker flushes any sessions that still have pending capture state, ensuring no events are lost if a capture hook was interrupted.

The broker fans out by file ownership. A capture started from one enabled repo can still write events into another enabled repo when the touched files belong there.
Cross-repo implementations are indexed separately in a global local database so
Semantica can map related work into a single implementation story without
changing the per-repo `lineage.db` schema.

## Capture

The `semantica capture` command is the primary ingestion path for AI agent activity. It is invoked by provider hooks (not by the user directly). Each provider registers hooks in its own configuration file (e.g., `.claude/settings.local.json`, `.cursor/hooks.json`) that call `semantica capture <provider> <hook-name>` with event metadata on stdin.

The capture command:

1. Parses the provider-specific stdin payload
2. On prompt-submit: saves the current transcript offset to `$SEMANTICA_HOME/capture/` so it knows where new content starts
3. On direct tool hooks: stores prompt, file edit, shell, and subagent boundary events immediately when the provider exposes them
4. On stop: reads the transcript or provider store from the saved offset, extracts events, and routes them through the broker to the correct repo's database
5. Packages prompt, step, and bundle provenance blobs for each completed turn

See [providers.md](providers.md) for provider-specific hook details.

## Git hooks

Semantica installs three Git hooks via `semantica enable`. Each hook invokes the `semantica` binary as a subprocess.

### pre-commit

Creates a pending checkpoint in the SQLite database. Writes a handoff file (`.semantica/.pre-commit-checkpoint`) containing the checkpoint ID and a timestamp. This file is how state is passed between the three hook phases.

The hook exits immediately - it never blocks the commit.

### commit-msg

Reads the handoff file. Appends the checkpoint trailer, and appends attribution and diagnostics trailers when the `trailers` setting is enabled:

```
Semantica-Checkpoint: chk_abc123
Semantica-Attribution: 42% claude_code (18/43 lines)
Semantica-Diagnostics: 3 files, lines: 15 exact, 2 modified, 1 formatted
```

If no AI matches the commit, the attribution trailer becomes `0% AI detected (0/N lines)` and the diagnostics trailer explains whether no AI events existed in the checkpoint window or whether events existed but did not match the committed files.

Trailers are only appended if the handoff file exists and is fresh (written within the last few seconds, to guard against stale state from aborted commits).

### post-commit

Reads the handoff file again. Links the commit hash to the pending checkpoint in the database. Deletes the handoff file.

By default, Semantica then spawns a detached background worker process:

```text
semantica worker run --repo <dir> --checkpoint <id> --commit <hash>
```

On macOS, users can optionally enable a launchd-backed path with
`semantica launcher enable`. In that mode, post-commit writes a repo-local job
marker and asks launchd to run `semantica worker drain`, which discovers and
processes pending markers across active repositories.

## Worker

The worker completes the checkpoint created by pre-commit. It can run in two
ways:

1. The default detached worker spawned directly by post-commit
2. The optional macOS launchd worker that drains pending markers

Both paths end up in the same `WorkerService.Run` pipeline for each checkpoint.

### Processing pipeline

1. **Session reconciliation** - Flushes any sessions that still have pending capture state (via `reconcileActiveSessions`). This is a catch-up mechanism - the primary capture path is the real-time `semantica capture` command triggered by provider hooks. The worker ensures no events are lost if a capture hook was interrupted or if the agent session outlived the hook call.

2. **Implementation reconciliation** - Processes pending broker observations in
   the global implementations database and attaches the current commit to a
   matching implementation when possible. This is how Semantica builds the
   local cross-repo implementation graph and its story-like timeline.

3. **File manifest** - Hashes every tracked file plus untracked, non-ignored files in the working tree using SHA-256. Compresses file contents with zstd and stores them in the content-addressed blob store. Records the manifest (path -> blob hash mapping) as a compressed JSON blob. Uses the previous checkpoint's manifest for incremental building.

4. **Checkpoint completion** - Marks the pending checkpoint as complete with the manifest hash and size.

5. **Session linking** - Finds sessions with events in the time window between the previous and current checkpoint. Associates them with the checkpoint in the database.

6. **AI attribution** - Diffs the commit against the parent. It first scores the current commit-linked checkpoint window, then applies bounded carry-forward for eligible created files that were already present in the previous commit-linked manifest but still scored 0 AI in the current window. For each changed line, it uses three match levels:
   - **Exact**: line matches AI output character-for-character
   - **Formatted**: match after normalizing whitespace
   - **Modified**: fuzzy match (line appears derived from AI output)

   Computes per-file and aggregate AI percentage and stores it on the checkpoint.

7. **Sync** (optional) - If the repo is connected, attempts a best-effort hosted sync for commit attribution and packaged turn provenance. Failures are logged but do not cause the worker to fail.

8. **Auto-playbook** (optional) - If enabled, runs `semantica _auto-playbook`
   in the background to generate a structured summary (title, intent, outcome,
   learnings, friction, keywords) and stores it on the checkpoint.

Steps 7 and 8 are best-effort - failures never cause the worker to fail.

## Storage

### SQLite database (`lineage.db`)

Single-file database in `.semantica/`. Contains:

| Table | Purpose |
|-------|---------|
| `repositories` | Repo records keyed by root path |
| `checkpoints` | Checkpoint metadata (ID, kind, trigger, status, timestamps) |
| `commit_links` | Maps commit hashes to checkpoint IDs |
| `agent_sources` | Provider source metadata keyed by provider and source key |
| `agent_sessions` | AI agent sessions (provider, model, timestamps, parent linkage) |
| `agent_events` | Captured prompt, assistant, tool, and provenance events |
| `provenance_manifests` | Per-turn packaged transcript/bundle metadata and upload state |
| `session_checkpoints` | Links sessions to the checkpoints they influenced |
| `checkpoint_stats` | Cached checkpoint aggregates |

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

Used for file snapshots (checkpoint manifests), event payloads, transcript
slices, prompts, and packaged provenance blobs.

### Settings (`settings.json`)

```json
{
  "enabled": true,
  "version": 1,
  "providers": ["claude-code", "cursor", "gemini", "copilot"],
  "connected": false,
  "connected_repo_id": "",
  "trailers": true,
  "automations": {
    "playbook": { "enabled": false }
  }
}
```

The `providers` field is a string array of installed hook provider names (not paths). `connected` controls whether the current repo attempts hosted sync. `connected_repo_id` is written when a repo is connected and stores the repo-local connection binding used by hosted features. The `trailers` field controls whether `Semantica-Attribution` and `Semantica-Diagnostics` are appended; `Semantica-Checkpoint` is always included. When omitted, `trailers` defaults to `true`.

### Global paths

| Purpose | Default path | Override |
| --- | --- | --- |
| Runtime state (broker registry, global objects, capture state) | `~/.semantica` | `SEMANTICA_HOME` |
| Global implementations index | `~/.semantica/implementations.db` | `SEMANTICA_HOME` |
| Launcher log (macOS launcher mode) | `~/.semantica/worker-launcher.log` | `SEMANTICA_HOME` |
| LaunchAgent plist (macOS launcher mode) | `~/Library/LaunchAgents/sh.semantica.worker.plist` | none |
| User config (auth fallback, release check cache) | `~/.config/semantica` | `XDG_CONFIG_HOME` |

Repo-local state still lives in `.semantica/` inside each enabled repository.

## Broker

The broker is a cross-repo event routing layer used by the `capture` command. It maintains a registry of enabled repositories at `repos.json` in the global Semantica directory, which defaults to `~/.semantica` and can be overridden via the `SEMANTICA_HOME` environment variable.

When an AI provider hook fires (e.g., Claude Code's `user-prompt-submit`), the capture command:

1. Reads the event payload from stdin
2. Looks up which registered repo(s) contain the affected files (deepest-match rule)
3. Routes the event to the matching repo database or databases
4. Records a lightweight observation in the global implementations index for
   later reconciliation

This allows Semantica to capture AI activity even when the provider's hook system doesn't know about the repo structure. In practice, a hook fired from one workspace can still route events into another Semantica-enabled repo if that repo owns the touched paths.

## Package structure

```
cmd/semantica/              CLI entrypoint (main.go)
internal/
  commands/                 Cobra command definitions
  launcher/                 Optional macOS launcher plumbing
  service/                  Core business logic
    worker.go               Background worker pipeline
    pre-commit.go           Pre-commit hook handler
    post-commit.go          Post-commit hook handler
    hook_commit_msg.go      Commit-msg hook handler
    rewind.go               Checkpoint rewind logic
    explain.go              Commit explanation and attribution
    implementations/        Cross-repo implementation services
    sessions.go             Session listing and details
    show.go                 Checkpoint detail display
    playbook.go             LLM playbook generation
    push.go                 Remote endpoint push
  store/impldb/             Global implementation index
  store/sqlite/             Storage layer
    schema/                 SQL schema definitions
    queries/                SQL query definitions
    db/                     sqlc-generated Go code
    store.go                Store implementation
  git/                      Git operations
    hooks.go                Hook script templates and installation
    diff.go                 Diff parsing
    log.go                  Log parsing
  hooks/                    AI provider integrations
    lifecycle.go            Event dispatch state machine
    state.go                Capture state management
    claude/                 Claude Code session ingestion
    cursor/                 Cursor session ingestion
    gemini/                 Gemini CLI session ingestion
    copilot/                GitHub Copilot session ingestion
  broker/                   Cross-repo event routing
  version/                  Build version injection
e2e/                        End-to-end tests
```
