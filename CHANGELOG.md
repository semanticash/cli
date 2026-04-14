# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

### Changed

### Fixed

## [0.3.2] - 2026-04-15

### Added

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
