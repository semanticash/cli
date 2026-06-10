# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.5.4] - 2026-06-10

### Added

- Added opt-in push-time intent-gap transport uploads, including `semantica set intent-gap <enabled|disabled>` and a non-blocking `pre-push` hook trigger.
- Added CLI helpers for intent-gap PR discovery and canonical payload hashing, matching the API upload contract.
- Added a repo-safe device identifier for intent-gap upload audit metadata; it is excluded from canonical payload hashing and deduplication.

### Fixed

- Existing enabled repos now refresh the `pre-push` hook when intent-gap analysis is turned on, so upgraded installs do not need to rerun `semantica enable`.
- Intent-gap uploads now map local LLM writer names to the API provider enum and log skipped or failed background uploads for `semantica doctor`.

### Changed

- `semantica enable` now installs the Semantica `pre-push` hook alongside the existing hooks while preserving user hook behavior.

## [0.5.3] - 2026-06-07

### Added

- Release artifacts now publish SLSA build provenance attestations signed via GitHub Actions OIDC and recorded in the Sigstore Rekor transparency log. See [SECURITY.md](https://github.com/semanticash/cli/blob/main/SECURITY.md#verifying-release-artifacts) for the manual verification recipe. Releases before `v0.5.3` continue to use SHA-256 checksum verification.

### Fixed

### Changed

## [0.5.2] - 2026-05-24

### Added

### Fixed

- Re-running `semantica connect` on an already connected repo no longer starts a provenance sync silently. Interactive terminals now show the pending local turn count and ask before syncing; non-interactive callers get an explanatory message and no upload side effect.
- File-edit step provenance now uses canonical hosted-diff shapes across Cursor, Gemini CLI, Kiro CLI, Kiro IDE, GitHub Copilot CLI, and Codex `apply_patch`, so synced turns can render provider code changes consistently.
- Provenance upload redaction now scans wrapped tool inputs and canonical multi-file `files[]` step blobs, including old/new text and non-object entries.
- Concurrent hook processes now write capture state through unique temp files, preventing Cursor file-edit attribution from being lost to corrupted capture-state JSON.
- User-facing read commands now wait longer for short-lived SQLite locks, avoiding spurious `SQLITE_BUSY` failures when they race with the post-commit worker.
- Codex tool events now inherit the active prompt turn before direct emission, so packaged provenance can include Codex tool steps for hosted `/diff` views.
- Post-commit provenance sync now drains newly packaged turn manifests immediately, so hosted diff data no longer waits for a later commit to upload.

### Changed

- `semantica status` and `semantica doctor` now explain pending local provenance turns and when they upload, so post-commit sync state is visible without re-running `connect`.

## [0.5.1] - 2026-05-20

### Added

- Added Codex as a playbook-generation writer. `semantica explain --generate`, the post-commit auto-playbook flow, and the commit/PR/implementation suggest commands now invoke `codex exec --skip-git-repo-check --output-last-message` when Codex is installed. Codex-only machines that previously failed with "no AI CLI found" now produce playbooks attributed to `codex`. The full fallback order is Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot CLI, Kiro CLI.

### Fixed

### Changed

- Replaced implicit `init()`-based provider registration with explicit registries for hook providers and writer LLMs. Production provider membership is centralized in `internal/providers/composition.go` (`NewHookRegistry`, `NewWriterRegistry`) and threaded through consumers via constructor arguments.

## [0.5.0] - 2026-05-17

### Added

- Added Codex provider installation groundwork: `semantica enable --providers codex` writes user-global Codex hooks under `$CODEX_HOME`, enables `[features] hooks = true`, stamps trusted hook hashes, preserves unrelated Codex hook entries and config values, and gates capture by the session's enabled repo before any broker/blob side effects.
- Added Codex hook capture for prompts and tool steps. `apply_patch` add/update records produce line-level attribution evidence, while deletions, empty-file adds, and rename-only halves produce provider-touch evidence without inflating line counts. Bash, Write, and Edit hook payloads are normalized through the shared direct-emit path.
- Added Codex support to cross-agent surfaces: `semantica skills install` now writes Semantica skills to `~/.codex/skills`, and `semantica handoff continue --agent codex` can launch `codex` with the saved handoff bundle when the binary is available.
- Registered Codex in provider discovery and canonical ordering.

### Fixed

- Codex `apply_patch` events now route reliably when emitted with repo-relative paths, including subdirectory sessions and delete-only patch sections, while attribution keys remain repo-relative.

### Changed

- `semantica blame` now shows attributing providers on AI file tags when known, `semantica blame --json` includes per-file provider involvement lists, and hosted attribution pushes preserve those lists for API commit-file responses.
- `semantica explain` now lists linked session providers in the AI involvement section.

## [0.4.1] - 2026-05-13

### Added

### Fixed

- Re-enabling Semantica now preserves previously wrapped user Git hooks and keeps user hooks blocking, while Semantica's own capture hook remains non-blocking.
- `semantica enable --providers` now rejects unknown provider names before creating local state or installing Git hooks.
- Git hook installation now writes through a temp file and platform-aware replacement to reduce partial-hook risk on interruption.
- Broker write failures are now mirrored to `hook-errors.log` so `semantica doctor` can report capture losses that previously only appeared in developer logs.

### Changed

- Provider-touch-only attribution is now reported as a separate provider-only sidecar instead of inflating headline AI line percentages. JSON, push payloads, commit trailers, and diagnostics expose the sidecar while keeping `ai_lines` and `ai_percentage` limited to exact, formatted, and modified line evidence.

## [0.4.0] - 2026-05-12

### Added

- `semantica handoff --write` creates a redacted `.semantica/handoff.md` bundle from the active Semantica-tracked agent session, the most recent persisted parent session when run between turns, or an explicitly named provider via `--from <provider>` for cross-agent handoff. When multiple providers are active, interactive terminals show a provider picker and non-interactive callers receive an enumerated `--from` hint. Bundles include recent user prompts, the last assistant message, file-touch context, recent commits, and uncommitted working-tree context for a fresh agent session. Interactive terminals can chain directly into `semantica handoff continue`; non-interactive callers receive a manual continue hint.
- `semantica handoff continue` can launch a fresh agent session with the saved handoff bundle for Claude Code, Cursor, Gemini CLI, Copilot CLI, and Kiro CLI when the matching binary is installed. `--print` emits a safe copyable command, and Kiro IDE receives a manual-launch hint because it has no CLI surface for this flow.
- Hidden `semantica skills handoff` backing command now shares the same writer as `semantica handoff --write`, preparing the CLI side of the `semantica-handoff` skill.
- `semantica skills install` fetches SKILL.md files from the protected `main` branch of the `semanticash/skills` GitHub repo and writes them into every detected agent skills directory (Claude Code, Cursor, Gemini CLI, Copilot CLI, Kiro). `--source <path>` overrides the network fetch with a local checkout for development and offline use. `semantica skills uninstall` removes Semantica-managed files from the same directories. Each installed file carries a versioned content hash; install `--force` overwrites destination conflicts, while uninstall `--force` only removes edited Semantica-managed files.
- Hidden `semantica skills explain <ref>` backing command now emits structured JSON for skill integrations, using local provenance when available, workspace API playbooks for connected repos, and a redacted git diff fallback otherwise.

### Fixed

- Handoff bundle assembly resolves provider session IDs to Semantica's local session IDs before reading `agent_events`, so real bundles include the captured prompt, assistant summary, and file-touch evidence.
- Handoff degraded-state notes now avoid raw database errors and absolute local paths in the generated markdown.

### Changed

- Handoff bundles now render recent prompts and assistant context in fenced blocks, prefer full prompt blobs over compact event summaries, include useful commit-message bodies, and hide Semantica metadata trailers from the commit list.
- Handoff writes always resolve the Git repository root before reading lineage data or writing `.semantica/handoff.md`, so subdirectory invocations target the correct repo.

## [0.3.9] - 2026-05-07

### Added

- `semantica doctor` now runs read-only local health checks for the CLI binary, PATH conflicts, launcher state, provider hooks, Git hooks, capture state, recent capture activity, hosted sync manifests, hook-error diagnostics, provider configuration risks, repo connection, and authentication, with styled terminal output plus plain text and JSON modes.
- Hook capture failures are now written to a bounded `hook-errors.log` sidecar so `semantica doctor` can report recent non-blocking hook failures.
- Per-file attribution results now include `evidence_classes`, a strongest-first list of all contributing evidence classes, while keeping `evidence_class` as the backwards-compatible display field.
- Kiro CLI can now be used as a fallback provider for playbook generation and other LLM-backed text features through `kiro-cli chat --no-interactive`.

### Fixed

- Provenance upload redaction now fails closed for prompts, bundles, step provenance, and unknown blob kinds instead of falling back to raw outbound content on redactor or JSON parsing failures.
- Tool input and response redaction now handles object, array, string, number, boolean, and null JSON shapes without treating valid non-object values as malformed.

### Changed

- Hosted sync manifest failure reasons now distinguish redaction failures from missing local blobs, with consistent `redaction failed: <kind>: <error>` messages for redaction drops.
- Commit evidence strength now accounts for weaker contributing fallback evidence, such as provider-touch or carry-forward signals, even when a stronger line-level class remains the primary display evidence.

## [0.3.8] - 2026-05-06

### Added

- Gemini CLI transcript support now handles both legacy JSON files and newer JSONL files with header session IDs.
- Gemini CLI 0.40+ subagent delegation is now captured from `invoke_agent` hooks, including the dispatched agent name and completion state.
- Kiro CLI 2.2 capture now installs a repo-local `semantica` agent profile with matched hooks for prompt, file-edit, shell, and session-boundary events.
- Kiro CLI 2.2 AgentCrew subagent dispatches and completion boundaries are now captured from `subagent` hooks.
- Kiro CLI AgentCrew child JSONL sessions are now discovered and replayed when discovery can link them unambiguously to a parent turn.
- Kiro IDE trace capture now emits line-level `Write` and `Edit` attribution for create, replace, and append actions when the trace includes old and new file content.
- Kiro IDE installs a `fileEdited` hook for incremental mid-turn capture, while keeping `agentStop` as the final sweep for missed events.

### Fixed

- Gemini CLI 0.40+ file-edit hook payloads are now normalized so `write_file` and `replace` events route to the correct repo and contribute line-level attribution.
- Gemini CLI transcript replay now resolves relative tool-call paths against the captured session working directory before routing replayed events.
- Copilot CLI `task` post-tool hooks no longer emit duplicate subagent completion events; `subagentStop` is now the canonical completion boundary.
- Provider hook settings are now written without HTML-escaping shell redirection characters, keeping generated hook commands readable in settings files.
- Kiro CLI 2.2 `write` and `shell` payloads are now normalized into `Write`, `Edit`, and `Bash` events with repo-relative paths resolved before routing, and direct `Write`/`Edit` events now use canonical `tool_uses` so their payload blobs contribute line-level attribution.
- Kiro CLI child JSONL replay now keeps trailing partial lines retryable instead of advancing offsets past malformed in-progress writes.
- Kiro IDE repeated edits to the same file in one execution now keep distinct event IDs by including Kiro action IDs in replay event identity.
- Kiro IDE rename events now carry file-touch evidence for the destination path.
- Kiro IDE hook installation now refreshes Semantica-owned hook files when their rendered definition changes, including the `patterns` field required by `fileEdited`.

### Changed

- Gemini CLI direct hooks and transcript replay now use the same provider session ID when JSONL transcripts expose a header session ID.
- Claude Code hook installation no longer registers the obsolete `PostToolUse[Task]` capture hook.
- Kiro CLI now treats direct `postToolUse` hooks as the parent capture source for file and shell operations; parent SQLite transcript replay stays disabled to avoid duplicate events with mismatched provider tool IDs.

## [0.3.7] - 2026-04-30

### Added

- Experimental Linux launcher backend using systemd user units, managed with the existing `semantica launcher enable`, `semantica launcher status`, and `semantica launcher disable` commands.
- Experimental Windows launcher backend using Task Scheduler, managed through the same launcher commands as macOS and Linux.
- Cross-platform launcher status reporting that shows settings, definition file presence, daemon-manager registration state, and log path with platform-appropriate hints for macOS, Linux, and Windows.

### Fixed

- Launcher-managed worker runs now capture standard output, standard error, and structured logger output consistently in the configured launcher log path, while per-repo drain output continues to land in `.semantica/worker.log`.
- Linux launcher units now handle spaced binary and log paths correctly, and repeated `launcher enable` runs now recognize already-registered idle units instead of reporting a fresh install every time.
- Windows launcher installs now accept native absolute executable paths, quote Task Scheduler arguments correctly for spaced paths, and classify scheduler errors more accurately during status and cleanup flows.
- Launcher help, status hints, and service-state reporting now reflect the actual backend on the current OS instead of falling back to macOS-only or cross-OS generic wording.

### Changed

- The launcher surface is now described in cross-platform terms such as `unit`, `service`, and `service target`, while keeping macOS, Linux, and Windows backends behind the same CLI workflow.
- Launcher registration state is now reported consistently across launchd, systemd, and Task Scheduler, so idle on-demand services no longer appear as drift just because they are between runs.
- Launcher settings now use `installed_unit_path` as the canonical key while continuing to read and temporarily write the legacy `installed_plist_path` key for compatibility with earlier installs.

## [0.3.6] - 2026-04-22

### Added

- Experimental macOS launcher-backed worker for agent-driven or IDE-assisted commit workflows, managed with `semantica launcher enable`, `semantica launcher status`, and `semantica launcher disable`.
- Launcher status reporting that shows user settings, plist presence, and launchd state side by side so drift is visible.

### Changed

- The post-commit worker can now run through an optional macOS launchd-backed path while keeping the default detached worker path for users who do not opt in.
- Per-repo worker output continues to land in `.semantica/worker.log` when the launcher is enabled, while launcher-level events are written to `~/.semantica/worker-launcher.log`.

### Fixed

- Re-push logging now uses a dedicated `re-push` prefix so enriched attribution logs are distinguishable from the initial remote push.

## [0.3.5] - 2026-04-21

### Added

- Permanent direct-hook contract fixtures for Claude Code, GitHub Copilot, Cursor, Gemini CLI, and Kiro CLI, including content-addressed blob checks to catch wire-shape regressions.
- Drift detection for direct-hook fixtures so stale or missing contract files fail test runs instead of silently reducing coverage.

### Changed

- Unified direct hook emission across Claude Code, GitHub Copilot, Cursor, Gemini CLI, and Kiro CLI around shared builder helpers while preserving provider-specific event shapes and payload contracts.
- Provider documentation now reflects current supported platform paths and detection behavior, including Windows coverage where applicable.
- Direct-hook contract tests now use `direct_emit_contract_test.go` naming to describe intent more clearly.

### Fixed

- Tightened direct-hook regression protection so payload/blob serialization changes are caught across all supported providers.
- Claude Code and Cursor project-path decoding now returns an empty path for source keys outside the provider project base, keeping emitted metadata consistent across Unix and Windows.
- Detached background workers now drop inherited loopback proxy settings before contacting Semantica or LLM endpoints, preventing agent-local proxies from breaking post-commit pushes while keeping real forward proxies intact.

## [0.3.4] - 2026-04-18

### Added

- Per-file attribution metadata in serialized results: `operation`, `classification`, and aggregate `ai_lines`
- Diagnostic `notes` arrays for both commit attribution and checkpoint-only attribution results

### Changed

- `semantica blame` prints a single Notes section sourced from the same diagnostics bundle used by serialized attribution output
- Raw transcript rendering reuses repeated payload blobs within a single request instead of reloading the same payload hash for every event
- Test fixtures and path examples were cleaned up to avoid user-specific absolute paths in the codebase

### Fixed

- Pure deletion paths appear consistently in per-file attribution results, even when only deletion metadata is available
- Missing blob payloads in attribution, transcript rendering, and provenance packaging emit warnings instead of failing silently
- Doctor bench recording surfaces local log-write failures instead of dropping them silently

## [0.3.3] - 2026-04-16

### Added

- `semantica disconnect` now notifies the dashboard via a best-effort `POST /v1/repos/{id}/disconnect` before clearing local settings

### Changed

- Claude Code hooks now install to `.claude/settings.local.json` instead of `.claude/settings.json`, keeping team-shared Claude config clean and avoiding hook pollution for teammates who don't use Semantica
- All provider hook commands are now wrapped with a shell guard that silently no-ops when `semantica` is not on PATH, preventing exec errors for teammates who clone a repo without Semantica installed
- `semantica agents` picker now matches `semantica enable` styling: bracket-style checkboxes in Semantica green, pre-selected installed agents, validation requiring at least one selection, and a post-change reload reminder
- Interactive prompts across `blame`, `explain`, `show`, `rewind`, `transcripts`, `impl`, and `agents` now print "Aborted by the user." on Ctrl+C instead of surfacing cryptic errors

### Fixed

## [0.3.2] - 2026-04-15

### Added

- `semantica suggest` commands now discover the Claude Code binary bundled inside the VS Code extension when the standalone CLI is not on PATH
- `semantica version` subcommand showing CLI version, Go version, and OS/Arch
- Agent reload instructions in README and provider docs for post-enable workflows
- Validation on agent selector requiring at least one agent to be selected
- Confirmation prompt before `semantica rewind` showing checkpoint details, linked commit, and impact warnings
- `--yes` / `-y` flag on `semantica rewind` to skip confirmation

### Changed

- Agent selector during `semantica enable` now uses bracket-style checkboxes with Semantica green for selected items
- Generic agent reload note replaces the Cursor-specific warning after `semantica enable`
- Ctrl+C during agent selection cleanly aborts without enabling

### Fixed

- Fixed Windows drive letter case mismatch in event routing (`C:` vs `c:` caused 0% attribution on Windows)
- Fixed console window flashing on Windows when the detached worker spawns git and LLM CLI subprocesses (added `CREATE_NO_WINDOW` flag)

## [0.3.1] - 2026-04-14

### Changed

- Detect Claude Code installed via VS Code extension or desktop app by checking for `~/.claude` directory when the CLI binary is not on PATH
- Reduce redundant database opens in implementation detail view by sharing a single lineage.db handle per repo for timeline, tokens, and attribution
- Batch commit subject lookups per repo instead of opening a git repo per commit

### Fixed

- Fix Windows CI workflow failing on Go toolchain temp file cleanup

## [0.3.0] - 2026-04-14

### Added

- Windows support: native builds for Windows amd64 and arm64
- Windows install via Scoop: `scoop install semanticash/semantica`
- Windows CI pipeline (`ci-windows.yml`) with compilation, unit test, and integration test gates
- `internal/platform` package for cross-platform file locking, process detachment, signal handling, and safe file rename
- CRLF normalization for git command output on Windows
- MSYS path normalization for Claude Code payloads on Windows (other providers use native paths and work without normalization)
- Windows clipboard support via `clip`
- Windows config path probes for Kiro CLI and Kiro IDE

### Fixed

- Fixed SQLite DSN construction to use `file:path` format instead of `file:///path`, which broke on Windows drive letter paths
- Fixed SQLite migration to use a single database handle instead of opening a second connection, which failed on Windows
- Fixed colon characters in capture state filenames on Windows (colons are forbidden in Windows filenames)
- Fixed absolute path detection for agent payload paths that use POSIX conventions on Windows hosts
- Fixed path separator mismatches in event routing and provenance normalization on Windows

## [0.2.3] - 2026-04-13

### Added
- Added attribution evidence notes to `blame` output when weaker methods are used
- Added per-file evidence classification to push payload for PR comments
- Added evaluation harness for attribution quality testing

### Changed

### Fixed
- Fixed carry-forward evidence for files that scored zero AI lines
- Fixed `git diff-tree` on root commits (first commit in a repo)
- Fixed symlink resolution in `findGitRoot` for macOS `/tmp` paths

## [0.2.2] - 2026-04-12

### Changed

- Refactored the attribution engine into dedicated `events`, `scoring`, `reporting`, and `carryforward` packages without changing the public CLI output.
- Unified commit attribution, checkpoint-only blame, and carry-forward logic around shared extraction and reporting paths to keep attribution behavior consistent across commands.
- Expanded attribution regression coverage to lock down public result shapes and provider-specific behavior during ongoing maintenance work.
- Reorganized the checkpoint worker into explicit preparation, reconciliation, enrichment, completion, and post-completion stages so checkpoint completion, attribution, and remote sync behavior follow clearer workflow boundaries.

### Fixed

- Reduced duplicated attribution code paths in the service layer, making commit and checkpoint attribution easier to maintain and less likely to drift over time.
- Resolves symlinks in findGitRoot so test comparisons match on systems with symlinked temp directories.

## [0.2.1] - 2026-04-10

### Added

- Auto-generated titles and summaries for cross-repo implementations when background worker activity expands an implementation across multiple repositories.
- A repo-local `auto-implementation-summary` automation setting, enabled by default, with `semantica set auto-implementation-summary <enabled|disabled>` for control.

### Changed

- Existing enabled repositories now backfill the new implementation-summary automation setting on first read so updated installs pick up the default without rerunning `semantica enable`.
- `semantica set` and `semantica status` now surface the auto-implementation-summary automation alongside the existing automation and trailer settings.
- Cross-repo implementation documentation now reflects the automatic title and summary flow, with `semantica suggest impl <id> --apply` positioned as the manual apply or override path.
- CI and checked-in generated database code are now pinned to `sqlc v1.30.0`.
- Added a dedicated implementations guide covering commands, states, boundaries, and JSON output.

### Fixed

- Background implementation summary generation now avoids duplicate work more reliably, clears in-progress markers on failure paths, and preserves unknown implementation metadata keys during updates.
- The post-commit implementation worker path now reuses its implementations database handle for auto-summary decisions instead of reopening the same store on the same commit path.
- Refreshed the `gitleaks` archive dependency chain to pull in the patched `rardecode` release and resolve the related Dependabot alert.

## [0.2.0] - 2026-04-10

### Added

- Cross-repo implementations as a new local-first view of AI-assisted work that spans multiple Semantica-enabled repositories.
- `semantica implementations` and `semantica impl` commands for listing and inspecting implementations with unified timelines across repos.
- Manual implementation management commands for closing, linking, and merging implementations.
- `semantica suggest implementations` for suggested titles, summaries, review priorities, and merge candidates.
- A global local implementations database plus broker observation capture and reconciliation for grouping related work across repositories.

### Changed

- The background worker now reconciles implementation observations and attaches commits to implementations without changing the existing `lineage.db` schema.
- `semantica tidy` now reports stale dormant implementations, unresolved implementation conflicts, and failed implementation observations, and can prune old reconciled observations on `--apply`.
- Implementation detail views now aggregate repo activity, commit history, and token usage into a single local-first timeline.

### Fixed

- Cross-repo implementation linking now preserves repo roles, branch context, and repo-local session coverage during manual linking and force-move operations.
- Implementation reconciliation and commit attachment now avoid nondeterministic matches and handle deferred parent-child session ordering more safely.

### Notes

- Implementations in `v0.2.0` are local-first. Hosted dashboard support can be added later without changing the local workflow.
- Existing repository `lineage.db` files and capture behavior remain backward-compatible.

## [0.1.0] - 2026-04-03

### Added

- Initial open-source release of the Semantica CLI.
- Local-first capture of AI-assisted development activity in Git repositories.
- Repository enablement with local Semantica state and Git hook installation.
- Commit attribution and blame reporting for AI-written, modified, and human-authored code.
- Commit explanation and playbook generation to summarize intent, outcome, and follow-up context.
- Checkpoint management, including listing and rewind support.
- Optional connect flow for syncing local provenance and attribution to a hosted workspace.
- Support for capturing activity from Claude Code, Cursor, Gemini CLI, Copilot CLI, Kiro IDE, and Kiro CLI.

### Notes
- Semantica stores capture data locally by default.
- Hosted features require an explicit repository connection.
