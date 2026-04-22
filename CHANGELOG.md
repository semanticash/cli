# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
