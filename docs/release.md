# Release Process

This document is for maintainers cutting Semantica releases. Normal contributors do not need it.

Semantica uses [GoReleaser](https://goreleaser.com/) to build, package, and publish releases.

## Targets

Releases are cross-compiled for:

| OS | Architecture | Archive |
|----|-------------|---------|
| macOS (darwin) | amd64, arm64 | tar.gz |
| Linux | amd64, arm64 | tar.gz |
| Windows | amd64, arm64 | zip |

Binaries are statically linked (`CGO_ENABLED=0`).

## Distribution channels

### GitHub Releases

Each tagged release creates a GitHub Release with:

- `semantica_<os>_<arch>.tar.gz` archives (`.zip` on Windows)
- `checksums.txt` (SHA-256)
- each archive includes:
  - the `semantica` binary
  - shell completion scripts for Bash, Zsh, and Fish
  - `LICENSE`
  - `README.md`
- release notes extracted from `CHANGELOG.md`

### Homebrew

GoReleaser pushes a cask update to `semanticash/homebrew-tap` on each release:

```bash
brew install semanticash/tap/semantica
```

The cask installs the `semantica` binary plus shell completions for Bash, Zsh, and Fish.

### Install script

`install.sh` downloads the latest release from GitHub, verifies the checksum, and installs the binary:

```bash
curl -fsSL https://semantica.sh/install.sh | sh
```

Supports `VERSION` and `INSTALL_DIR` environment variables for pinning.

### Scoop (Windows)

GoReleaser pushes a manifest to `semanticash/scoop-bucket` on each release:

```powershell
scoop bucket add semanticash https://github.com/semanticash/scoop-bucket
scoop install semantica
```

## Creating a release

1. Ensure `main` is clean and all CI checks pass.
2. Add a matching section to `CHANGELOG.md` for the release version:

```md
## [0.1.1] - 2026-03-15

### Added

### Changed

### Fixed
```

Write release notes as user-facing bullets grouped under `Added`, `Changed`, and `Fixed`. Do not paste raw commit hashes or a commit-by-commit dump.

3. Tag the release:

```bash
git tag -a v0.1.1 -m "v0.1.1"
git push origin v0.1.1
```

4. GoReleaser runs via GitHub Actions on tag push. It:
   - Builds binaries for all targets
   - Creates the GitHub Release with archives and checksums
   - Updates the Homebrew tap cask
   - Extracts the matching `CHANGELOG.md` entry and uses it as the GitHub release body

If the release workflow cannot find a `CHANGELOG.md` section for the tag version, it fails instead of publishing raw commit-message notes.

## Version injection

The version metadata is injected at build time via linker flags:

```
-X github.com/semanticash/cli/internal/version.Version=<version>
-X github.com/semanticash/cli/internal/version.Commit=<short_commit>
```

`make build` uses `git describe --tags --always --dirty` for the version and `git rev-parse --short HEAD` for the commit. GoReleaser uses the tag version and short commit for releases.

`semantica --version` prints the injected version and commit.

## CI checks

Every push and PR runs these checks (see `.github/workflows/ci.yml`):

| Job | What it does |
|-----|-------------|
| `generated` | Regenerates sqlc code and checks for drift |
| `test` | Unit tests with race detector |
| `lint` | golangci-lint v2.11.3, installed with the job's Go toolchain |
| `build` | GoReleaser cross-compile check (`--snapshot`) |
| `e2e` | End-to-end tests against compiled binary |
| `shellcheck` | Validates `install.sh` |

## Configuration

Release configuration lives in `.goreleaser.yaml`. Key settings:

- Archives use `semantica_<os>_<arch>` naming
- `make completions` runs before release to generate shell completion scripts
- Release archives bundle the generated completion scripts plus `LICENSE` and `README.md`
- GitHub release notes are extracted from the matching `CHANGELOG.md` section during the release workflow
- Homebrew cask installs Bash, Zsh, and Fish completions from the bundled `completions/` files
- Homebrew cask updates require the `HOMEBREW_TAP_TOKEN` secret for tap repo access
