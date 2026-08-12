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

Creates a pending internal lineage record, implemented as a checkpoint row in the SQLite database. Writes a handoff file (`.semantica/.pre-commit-checkpoint`) containing the record ID and a timestamp. This file passes state between the three hooks.

The hook exits immediately - it never blocks the commit.

### commit-msg

Reads the handoff file. Appends the lineage trailer, and appends attribution and diagnostics trailers when the `trailers` setting is enabled:

```
Semantica-Checkpoint: chk_abc123
Semantica-Attribution: 42% claude_code (18/43 lines)
Semantica-Diagnostics: 3 files, lines: 15 exact, 2 modified, 1 formatted
```

If no AI matches the commit, the attribution trailer becomes `0% AI detected (0/N lines)` and the diagnostics trailer explains whether no AI events existed in the lineage window or whether events existed but did not match the committed files.

Trailers are only appended if the handoff file exists and is fresh (written within the last few seconds, to guard against stale state from aborted commits).

### post-commit

Reads the handoff file again. Links the commit hash to the pending lineage record in the database. Deletes the handoff file.

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

2. **File manifest** - Hashes every tracked file plus untracked, non-ignored files in the working tree using SHA-256. Compresses file contents with zstd and stores them in the content-addressed blob store. Records the manifest (path -> blob hash mapping) as a compressed JSON blob. Uses the previous lineage record's manifest for incremental building.

3. **Lineage completion** - Marks the pending record as complete with the manifest hash and size.

4. **Session linking** - Finds sessions with events in the time window between the previous and current lineage records. Associates them with the record in the database.

5. **AI attribution** - Diffs the commit against the parent. It first scores the current commit-linked lineage window, then applies bounded carry-forward for eligible created or modified files that still scored 0 AI in the current window. Historical evidence must still match the current diff before it is credited. For each changed line, it uses three match levels:
   - **Exact**: line matches AI output character-for-character
   - **Formatted**: match after normalizing whitespace
   - **Modified**: fuzzy match (line appears derived from AI output)

   Computes per-file and aggregate AI percentage and stores it on the lineage record. Optional v2 scoring aligns verified tool deltas with committed lines and preserves unaligned changes as file-level evidence. Provider-touch-only lines are carried as `ai_provider_only_lines` and excluded from the headline AI percentage. Per-file results include a primary display evidence class plus the full list of contributing evidence classes so exact line matches, provider-touch fallback, carry-forward, and deletion signals remain distinguishable.

6. **Sync** (optional) - If the repo is connected, attempts a best-effort hosted sync for commit attribution and packaged turn provenance. Failures are logged but do not cause the worker to fail.

7. **Auto-playbook** (optional) - If enabled, runs `semantica _auto-playbook`
   in the background to generate a structured summary (title, intent, outcome,
   learnings, friction, keywords) and stores it on the lineage record.

Steps 6 and 7 are best-effort - failures never cause the worker to fail.

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

Used for file snapshots (lineage manifests), event payloads, transcript
slices, prompts, and packaged provenance blobs.

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

Claude Code pre- and post-Bash hooks capture canonical deltas and link them to
their tool events. Recovery runs during worker drains and with
`semantica tidy --apply`. Optional v2 scoring verifies and aligns these deltas
against committed lines; partial or ambiguous evidence remains unscored.

### Settings (`settings.json`)

```json
{
  "enabled": true,
  "version": 1,
  "providers": ["claude-code", "cursor", "gemini", "copilot"],
  "connected": false,
  "connected_repo_id": "",
  "trailers": true,
  "attribution_v2": false,
  "automations": {
    "playbook": { "enabled": false }
  }
}
```

The `providers` field lists installed hook providers. `connected` controls hosted sync, and `connected_repo_id` stores the repository binding. `trailers` controls the optional attribution and diagnostics trailers; `Semantica-Checkpoint` is always included. `attribution_v2` enables experimental tool-delta scoring and defaults to `false`. `SEMANTICA_ATTRIBUTION_V2` can override it with `1`, `true`, `0`, or `false`.

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

```
cmd/semantica/              CLI entrypoint (main.go)
internal/
  commands/                 Cobra command definitions
  launcher/                 Optional cross-platform launcher plumbing
  service/                  Core business logic
    worker.go               Background worker pipeline
    pre-commit.go           Pre-commit hook handler
    post-commit.go          Post-commit hook handler
    hook_commit_msg.go      Commit-msg hook handler
    rewind.go               Internal checkpoint restore support
    explain.go              Commit explanation and attribution
    sessions.go             Session listing and details
    show.go                 Lineage record detail display
    playbook.go             LLM playbook generation
    push.go                 Remote endpoint push
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
  toolsnap/                 Isolated workspace snapshot and tree-diff engine
  broker/                   Cross-repo event routing
  version/                  Build version injection
e2e/                        End-to-end tests
```
