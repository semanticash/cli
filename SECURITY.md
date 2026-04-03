# Security Policy

## Reporting a vulnerability

If you discover a security vulnerability in Semantica, please report it responsibly. **Do not open a public GitHub issue.**

Email: **security@semantica.sh**

Include:

- Description of the vulnerability
- Steps to reproduce
- Impact assessment (if known)
- Your contact information for follow-up

We will acknowledge receipt within 48 hours and aim to provide an initial assessment within 5 business days.

## Scope

This policy covers the Semantica CLI (`semanticash/cli`) and its interactions with:

- Local filesystem (`.semantica/` directory, blob store, SQLite database)
- Git hooks (pre-commit, commit-msg, post-commit)
- AI provider session data (read-only ingestion)
- Authentication credentials (OS keychain when available, file fallback at `~/.config/semantica/credentials.json`)
- Remote API communication (when configured)

## Security model

### Local-first design

All Semantica data is stored locally in `.semantica/` within the repository. No data is sent to external services unless the user authenticates with `semantica auth login`, provides `SEMANTICA_API_KEY`, or sets `SEMANTICA_ENDPOINT`.

### Outbound redaction

- LLM prompts and remote attribution payloads are passed through best-effort secret redaction before they leave the machine.
- `remote_url` values are sanitized before upload so embedded credentials, query strings, and fragments are not sent upstream.
- Redaction does not rewrite local raw capture in `.semantica/`.
- If the redactor cannot initialize, the affected outbound operation fails closed instead of sending raw content.

### Credential storage

- OAuth tokens are stored in OS secure storage when available (macOS Keychain, Linux Secret Service / libsecret-compatible keyring).
- On headless or CI environments without a running keyring service, credentials fall back to `~/.config/semantica/credentials.json` with `0600` file permissions (owner read/write only).
- Existing file credentials are automatically migrated to the secure store when it becomes available.
- The `SEMANTICA_API_KEY` environment variable is supported for CI environments where neither secure storage nor file-based credentials are practical.
- Credentials are never written to `.semantica/` or committed to Git.

### Git hooks

Hooks installed by `semantica enable` are standard shell scripts in `.git/hooks/`. They:

- Run only when `.semantica/enabled` exists
- Do not perform heavy attribution or network work in the foreground commit path - the background worker is fully detached
- Never modify repository content beyond appending trailers to the commit message
- Are inert after `semantica disable`

### AI provider data

Semantica installs lightweight hooks in each detected provider's configuration file (e.g., `.claude/settings.json`, `.cursor/hooks.json`) to capture agent activity in real time. It never modifies provider session logs or transcript files. Installed provider names are recorded in `.semantica/settings.json`.

### Content-addressed blob store

File snapshots in `.semantica/objects/` are stored using SHA-256 content addressing with zstd compression. Blob integrity can be verified by recomputing the hash.

## Supported versions

Security fixes are applied to the latest release. We do not backport fixes to older versions.

| Version | Supported |
|---------|-----------|
| Latest  | Yes       |
| Older   | No        |

## Disclosure policy

We follow coordinated disclosure. Once a fix is released, we will:

1. Publish a GitHub security advisory
2. Credit the reporter (unless they prefer anonymity)
3. Include fix details in the release notes
