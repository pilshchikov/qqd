#!/bin/sh
# Install qqd.
#
# From GitHub releases (run anywhere, or curl | sh):
#   curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh -s -- -d /usr/local/bin
#   curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh -s -- -v v2026.05.24.42
#
# From source, clone the repo and run:
#   make install
set -e

REPO="pilshchikov/qqd"
INSTALL_DIR="${HOME}/.local/bin"
VERSION=""
SKIP_VERIFY=0

# Parse arguments
while [ $# -gt 0 ]; do
    case "$1" in
        -d|--dir)       INSTALL_DIR="$2"; shift 2 ;;
        -v|--version)   VERSION="$2"; shift 2 ;;
        --no-verify)    SKIP_VERIFY=1; shift ;;
        -h|--help)
            echo "Usage: install.sh [-d <dir>] [-v <version>] [--no-verify]"
            echo "  -d, --dir       Install directory (default: ~/.local/bin)"
            echo "  -v, --version   Version to install from GitHub (default: latest)"
            echo "  --no-verify     Skip checksum verification of the downloaded binary."
            echo "                  Not recommended; use only when checksums.txt is unreachable."
            echo ""
            echo "Downloads the latest release from GitHub by default and verifies its"
            echo "SHA-256 checksum against checksums.txt published with the release."
            echo "For source installs, clone the repo and run: make install"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      echo "error: unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)   ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    *)               echo "error: unsupported architecture: $ARCH"; exit 1 ;;
esac

# Resolve version
if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    if [ -z "$VERSION" ]; then
        echo "error: could not determine latest version"
        exit 1
    fi
fi

BINARY="qqd_${OS}_${ARCH}"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY}"

echo "installing qqd ${VERSION} (${OS}/${ARCH})..."
echo "  from: ${URL}"
echo "  to:   ${INSTALL_DIR}/qqd"

# Create install directory
mkdir -p "$INSTALL_DIR"

# Download and extract
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

if ! curl -fsSL "$URL" -o "${TMPDIR}/${BINARY}"; then
    echo "error: download failed — check that version ${VERSION} exists for ${OS}/${ARCH}"
    exit 1
fi

# Verify checksum unless explicitly disabled. checksums.txt is published
# alongside each release binary and contains lines like:
#   <sha256>  qqd_<os>_<arch>
# We pick the line for our binary, recompute the hash locally, and compare.
if [ "$SKIP_VERIFY" -eq 1 ]; then
    echo "warning: --no-verify set; skipping checksum verification of ${BINARY}"
else
    CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
    if ! curl -fsSL "$CHECKSUMS_URL" -o "${TMPDIR}/checksums.txt"; then
        echo "error: could not download checksums.txt from ${CHECKSUMS_URL}"
        echo "       (re-run with --no-verify to skip if the file is genuinely unreachable)"
        exit 1
    fi
    EXPECTED=$(awk -v binary="$BINARY" '$2 == binary { print $1; exit }' "${TMPDIR}/checksums.txt")
    if [ -z "$EXPECTED" ]; then
        echo "error: ${BINARY} not listed in checksums.txt; release may be incomplete"
        exit 1
    fi
    # Pick whichever sha256 tool is available. shasum is on macOS by default;
    # sha256sum is on most Linux distributions.
    if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL=$(sha256sum "${TMPDIR}/${BINARY}" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        ACTUAL=$(shasum -a 256 "${TMPDIR}/${BINARY}" | awk '{print $1}')
    else
        echo "error: no sha256sum/shasum available for verification (re-run with --no-verify to skip)"
        exit 1
    fi
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        echo "error: checksum mismatch for ${BINARY}"
        echo "       expected: ${EXPECTED}"
        echo "       actual:   ${ACTUAL}"
        echo "       refusing to install. The download may be corrupted or tampered with."
        exit 1
    fi
    echo "verified ${BINARY} sha256: ${ACTUAL}"
fi

install -m 755 "${TMPDIR}/${BINARY}" "${INSTALL_DIR}/qqd"

echo "installed qqd ${VERSION} to ${INSTALL_DIR}/qqd"

# Check if install dir is in PATH
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "note: add ${INSTALL_DIR} to your PATH" ;;
esac
