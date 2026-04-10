#!/bin/sh
set -e

REPO="semanticash/cli"

# --- Detect OS and architecture ---

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Error: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) echo "Error: unsupported OS: $OS" >&2; exit 1 ;;
esac

# --- Resolve version ---

if [ -z "$VERSION" ]; then
  VERSION=$(curl -sSf "https://api.github.com/repos/${REPO}/releases/latest" |
    grep '"tag_name"' | sed 's/.*"v\(.*\)".*/\1/')
  if [ -z "$VERSION" ]; then
    echo "Error: could not determine latest version" >&2
    exit 1
  fi
fi

# --- Resolve install directory ---

if [ -z "$INSTALL_DIR" ]; then
  # Prefer well-known system directories over arbitrary writable PATH entries.
  for dir in /usr/local/bin /usr/bin /opt/homebrew/bin; do
    if [ -d "$dir" ] && [ -w "$dir" ]; then
      INSTALL_DIR="$dir"
      break
    fi
  done
fi

if [ -z "$INSTALL_DIR" ]; then
  INSTALL_DIR="$HOME/.local/bin"
  mkdir -p "$INSTALL_DIR"

  # Check if the fallback is on PATH.
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "Warning: $INSTALL_DIR is not on your PATH. Add it to use semantica." >&2 ;;
  esac
fi

# --- Download ---

FILENAME="semantica_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${VERSION}/${FILENAME}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading semantica v${VERSION} for ${OS}/${ARCH}..."
curl -sSfL "$URL" -o "${TMPDIR}/${FILENAME}"

# --- Verify checksum ---

CHECKSUMS_URL="https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"
curl -sSfL "$CHECKSUMS_URL" -o "${TMPDIR}/checksums.txt"

EXPECTED=$(grep "${FILENAME}" "${TMPDIR}/checksums.txt" | awk '{print $1}')
if command -v sha256sum > /dev/null 2>&1; then
  ACTUAL=$(sha256sum "${TMPDIR}/${FILENAME}" | awk '{print $1}')
elif command -v shasum > /dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "${TMPDIR}/${FILENAME}" | awk '{print $1}')
else
  echo "Warning: cannot verify checksum (no sha256sum or shasum found)" >&2
  ACTUAL="$EXPECTED"
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "Error: checksum mismatch" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  got:      $ACTUAL" >&2
  exit 1
fi

# --- Extract and install ---

echo "Extracting..."
tar -xzf "${TMPDIR}/${FILENAME}" -C "$TMPDIR"

echo "Installing to ${INSTALL_DIR}..."
mkdir -p "$INSTALL_DIR"
install -m 0755 "${TMPDIR}/semantica" "${INSTALL_DIR}/semantica"

echo "semantica v${VERSION} installed to ${INSTALL_DIR}/semantica"
"${INSTALL_DIR}/semantica" --version
