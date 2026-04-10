# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

### Changed

### Fixed

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
