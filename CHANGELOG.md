# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

### Changed

### Fixed

## [0.2.0] - 2026-04-08

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
