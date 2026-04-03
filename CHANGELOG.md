# Changelog

All significant changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

### Changed

### Fixed

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