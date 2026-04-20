#!/usr/bin/env bash
set -euo pipefail

REPO="paarths-collab/killport"
VERSION="${KILLPORT_VERSION:-}"

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: required command not found: $1" >&2
        exit 1
    fi
}

need_cmd curl
need_cmd tar

if [ -z "$VERSION" ]; then
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -n1)"
fi

if [ -z "$VERSION" ]; then
    echo "error: could not determine latest release version" >&2
    echo "hint: set KILLPORT_VERSION manually, e.g. KILLPORT_VERSION=1.0.2" >&2
    exit 1
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
    linux|darwin) ;;
    *)
        echo "error: unsupported OS: $OS" >&2
        echo "hint: this install script supports Linux and macOS" >&2
        exit 1
        ;;
esac

case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
        echo "error: unsupported architecture: $ARCH" >&2
        exit 1
        ;;
esac

FILENAME="killport_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${VERSION}/${FILENAME}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading killport v${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "$URL" -o "$TMP_DIR/killport.tar.gz"
tar -xzf "$TMP_DIR/killport.tar.gz" -C "$TMP_DIR"

if [ ! -f "$TMP_DIR/killport" ]; then
    echo "error: release archive does not contain expected binary: killport" >&2
    exit 1
fi

chmod +x "$TMP_DIR/killport"

INSTALL_DIR="/usr/local/bin"
if [ -w "$INSTALL_DIR" ]; then
    mv "$TMP_DIR/killport" "$INSTALL_DIR/killport"
else
    need_cmd sudo
    sudo mv "$TMP_DIR/killport" "$INSTALL_DIR/killport"
fi

echo "Installed successfully: ${INSTALL_DIR}/killport"
echo "Run: killport --help"