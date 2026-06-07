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

## Verifying release artifacts

Starting with `v0.5.3`, Semantica releases publish SLSA build provenance attestations signed by GitHub Actions OIDC and recorded in the public Sigstore [Rekor](https://docs.sigstore.dev/logging/overview/) transparency log. An attestation lets you verify that a downloaded artifact was built by the Semantica release workflow at a specific tag, not only that it matches the same-release `checksums.txt`.

### Quick verification

You need [GitHub CLI](https://cli.github.com/) `gh >= 2.67.0`. Earlier versions (`2.49.0` through `< 2.67.0`) are affected by [CVE-2025-25204](https://nvd.nist.gov/vuln/detail/CVE-2025-25204), which can cause `gh attestation verify` to return success when no attestation is present. Check with `gh --version`.

Download the artifact and verify it, replacing the version and architecture as needed:

```sh
gh release download v0.5.3 \
  --repo semanticash/cli \
  --pattern 'semantica_linux_amd64.tar.gz'

gh attestation verify ./semantica_linux_amd64.tar.gz \
  --repo semanticash/cli \
  --signer-workflow semanticash/cli/.github/workflows/release.yml \
  --source-ref refs/tags/v0.5.3
```

A successful verification confirms the artifact was produced by the named workflow at the named tag. Pinning `--signer-workflow` and `--source-ref` (instead of only `--owner`) ensures the attestation came from the release workflow and tag you expected.

### Scope and limitations

- **Releases before `v0.5.3` have no attestation.** They install via SHA-256 checksum verification as before; the verification command above will fail for older tags. This is expected.
- **Homebrew and Scoop installs do not consume these attestations yet.** Their trust model continues to be the tap or bucket manifest commit.
- **The default `install.sh` path is unchanged in `v0.5.3`.** It still verifies SHA-256 against `checksums.txt`. Automatic installer-side attestation verification may be added in a future release; today you can run the command above manually.
- **Maintainer account compromise is not covered.** An attacker who can edit the release workflow or push tags can produce a valid attestation for malicious artifacts. Branch protection on `.github/workflows/release.yml` and signed-commit enforcement are separate hardenings.

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
