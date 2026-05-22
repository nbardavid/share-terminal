#!/bin/sh
# install.sh — installs `control` from the latest GitHub Release.
#
# Usage:
#   curl -sSfL https://github.com/nbardavid/share-terminal/releases/latest/download/install.sh | sh
#
# Installs into ~/.local/bin by default (no sudo). If that directory is not
# in your PATH, the script tells you the line to add to your shell rc.
#
# Supported environment variables:
#   INSTALL_DIR=/path    # default: ~/.local/bin
#   VERSION=vX.Y.Z       # default: latest release ("latest")

set -eu

REPO="nbardavid/share-terminal"
BIN="control"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"
VERSION="${VERSION:-latest}"

# --- OS / arch detection ---
OS="$(uname -s)"
case "${OS}" in
    Linux*)  OS=linux ;;
    Darwin*) OS=darwin ;;
    *) echo "Unsupported OS: ${OS}" >&2; exit 1 ;;
esac

ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

ASSET="${BIN}-${OS}-${ARCH}"

if [ "${VERSION}" = "latest" ]; then
    URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
    CHECKSUM_URL="https://github.com/${REPO}/releases/latest/download/checksums.txt"
else
    URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
    CHECKSUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
fi

# --- download ---
TMP="$(mktemp -t control-install.XXXXXX)"
trap 'rm -f "${TMP}" "${TMP}.sum"' EXIT INT TERM

echo "-> Downloading ${ASSET}..."
if ! curl -fsSL "${URL}" -o "${TMP}"; then
    echo "Download failed from ${URL}" >&2
    exit 1
fi

# --- SHA-256 verification ---
if curl -fsSL "${CHECKSUM_URL}" -o "${TMP}.sum" 2>/dev/null; then
    EXPECTED="$(grep " ${ASSET}\$" "${TMP}.sum" | awk '{print $1}' || true)"
    if [ -n "${EXPECTED}" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            GOT="$(sha256sum "${TMP}" | awk '{print $1}')"
        elif command -v shasum >/dev/null 2>&1; then
            GOT="$(shasum -a 256 "${TMP}" | awk '{print $1}')"
        else
            GOT=""
        fi
        if [ -n "${GOT}" ] && [ "${GOT}" != "${EXPECTED}" ]; then
            echo "SHA-256 mismatch! File is corrupt or has been tampered with." >&2
            echo "  expected: ${EXPECTED}" >&2
            echo "  got:      ${GOT}" >&2
            exit 1
        fi
        if [ -n "${GOT}" ]; then
            echo "SHA-256 verified."
        fi
    fi
fi

chmod +x "${TMP}"

# --- install (no sudo: writes into the home directory) ---
mkdir -p "${INSTALL_DIR}"
DEST="${INSTALL_DIR}/${BIN}"
mv "${TMP}" "${DEST}"
trap - EXIT INT TERM

echo "Installed: ${DEST}"

# --- PATH check, advise per shell ---
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*)
        echo
        "${DEST}" --version
        echo
        echo "Ready. Run \`control --help\` to get started."
        ;;
    *)
        echo
        echo "${INSTALL_DIR} is not in your \$PATH."
        echo "   Add this line to your shell config:"
        echo
        SHELL_NAME="$(basename "${SHELL:-sh}")"
        case "${SHELL_NAME}" in
            bash)
                echo "       echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
                ;;
            zsh)
                echo "       echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
                ;;
            fish)
                echo "       fish_add_path \$HOME/.local/bin"
                ;;
            *)
                echo "       export PATH=\"\$HOME/.local/bin:\$PATH\""
                echo "   (add to the rc file of ${SHELL_NAME})"
                ;;
        esac
        echo
        echo "   Or run the binary directly: ${DEST} --help"
        ;;
esac
