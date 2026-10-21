#!/bin/sh
# killport installer. Works with any POSIX sh (dash, bash, busybox ash):
#   curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh
# Set KILLPORT_VERSION (e.g. 1.1.0) to install a specific release.
set -eu

REPO="paarths-collab/killport"
VERSION="${KILLPORT_VERSION:-}"
INSTALL_DIR="/usr/local/bin"

err() {
    echo "error: $*" >&2
}

die() {
    err "$@"
    exit 1
}

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        die "required command not found: $1"
    fi
}

USE_SUDO=0
as_root() {
    if [ "$USE_SUDO" -eq 1 ]; then
        sudo "$@"
    else
        "$@"
    fi
}

need_cmd curl
need_cmd tar
need_cmd uname
need_cmd mktemp

# Pick a SHA-256 tool up front so we fail before downloading anything.
if command -v sha256sum >/dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
    err "no SHA-256 tool found (need sha256sum or shasum)"
    die "refusing to install an unverified binary"
fi

if [ -z "$VERSION" ]; then
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)" || VERSION=""
fi
VERSION="${VERSION#v}"

if [ -z "$VERSION" ]; then
    err "could not determine latest release version"
    die "hint: set KILLPORT_VERSION manually, e.g. KILLPORT_VERSION=1.1.0"
fi

case "$VERSION" in
    *[!0-9A-Za-z._-]*) die "invalid version string: $VERSION" ;;
esac

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
    linux|darwin) ;;
    *)
        err "unsupported OS: $OS"
        die "hint: this install script supports Linux and macOS"
        ;;
esac

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "unsupported architecture: $ARCH" ;;
esac

FILENAME="killport_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/v${VERSION}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Downloading killport v${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "${BASE_URL}/${FILENAME}" -o "$TMP_DIR/$FILENAME" ||
    die "failed to download ${BASE_URL}/${FILENAME}"
curl -fsSL "${BASE_URL}/checksums.txt" -o "$TMP_DIR/checksums.txt" ||
    die "failed to download ${BASE_URL}/checksums.txt (cannot verify archive)"

# checksums.txt lines look like: <sha256>  <archive name>
EXPECTED="$(awk -v f="$FILENAME" '$2 == f || $2 == "*" f { print $1; exit }' "$TMP_DIR/checksums.txt")"
if [ -z "$EXPECTED" ]; then
    die "no checksum entry for ${FILENAME} in checksums.txt; refusing to install"
fi

ACTUAL="$(sha256_of "$TMP_DIR/$FILENAME")" || ACTUAL=""
EXPECTED="$(printf '%s' "$EXPECTED" | tr 'A-F' 'a-f')"
ACTUAL="$(printf '%s' "$ACTUAL" | tr 'A-F' 'a-f')"
if [ -z "$ACTUAL" ] || [ "$ACTUAL" != "$EXPECTED" ]; then
    err "checksum mismatch for ${FILENAME}"
    err "  expected: ${EXPECTED}"
    err "  actual:   ${ACTUAL:-<could not compute>}"
    die "refusing to install"
fi
echo "Checksum OK (sha256 ${ACTUAL})"

tar -xzf "$TMP_DIR/$FILENAME" -C "$TMP_DIR" || die "failed to extract ${FILENAME}"

if [ ! -f "$TMP_DIR/killport" ]; then
    die "release archive does not contain expected binary: killport"
fi

chmod +x "$TMP_DIR/killport"

# Create the install dir if missing; use sudo only when we cannot write to it.
if ! mkdir -p "$INSTALL_DIR" 2>/dev/null || [ ! -w "$INSTALL_DIR" ]; then
    need_cmd sudo
    USE_SUDO=1
    as_root mkdir -p "$INSTALL_DIR" || die "could not create ${INSTALL_DIR}"
fi

as_root mv "$TMP_DIR/killport" "$INSTALL_DIR/killport" ||
    die "could not install to ${INSTALL_DIR}/killport"

echo "Installed successfully: ${INSTALL_DIR}/killport"
if ! "$INSTALL_DIR/killport" --version; then
    echo "warning: installed binary did not report its version (--version failed)" >&2
fi
echo "Run: killport --help"
