#!/bin/sh
# install.sh — installe `control` depuis la dernière GitHub Release.
#
# Usage :
#   curl -sSf https://raw.githubusercontent.com/nbardavid/share-terminal/main/install.sh | sh
#
# Variables d'environnement supportées :
#   INSTALL_DIR=/path   # défaut : /usr/local/bin (sudo si pas writable)
#   VERSION=vX.Y.Z      # défaut : dernière release ("latest")

set -eu

REPO="nbardavid/share-terminal"
BIN="control"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-latest}"

# --- détection OS / arch ---
OS="$(uname -s)"
case "${OS}" in
    Linux*)  OS=linux ;;
    Darwin*) OS=darwin ;;
    *) echo "OS non supporté : ${OS}" >&2; exit 1 ;;
esac

ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "Architecture non supportée : ${ARCH}" >&2; exit 1 ;;
esac

ASSET="${BIN}-${OS}-${ARCH}"

if [ "${VERSION}" = "latest" ]; then
    URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
    CHECKSUM_URL="https://github.com/${REPO}/releases/latest/download/checksums.txt"
else
    URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
    CHECKSUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
fi

# --- téléchargement ---
TMP="$(mktemp -t control-install.XXXXXX)"
trap 'rm -f "${TMP}" "${TMP}.sum"' EXIT INT TERM

echo "→ Téléchargement de ${ASSET}..."
if ! curl -fsSL "${URL}" -o "${TMP}"; then
    echo "Échec du téléchargement depuis ${URL}" >&2
    exit 1
fi

# --- vérification SHA-256 (best-effort, n'échoue pas si checksums.txt absent) ---
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
            echo "Checksum SHA-256 incorrect ! Le fichier téléchargé est corrompu ou compromis." >&2
            echo "  attendu : ${EXPECTED}" >&2
            echo "  obtenu  : ${GOT}" >&2
            exit 1
        fi
        if [ -n "${GOT}" ]; then
            echo "✓ SHA-256 vérifié."
        fi
    fi
fi

chmod +x "${TMP}"

# --- installation ---
DEST="${INSTALL_DIR}/${BIN}"
echo "→ Installation dans ${DEST}..."

if [ -w "${INSTALL_DIR}" ]; then
    mv "${TMP}" "${DEST}"
elif command -v sudo >/dev/null 2>&1; then
    echo "  (sudo requis pour écrire dans ${INSTALL_DIR})"
    sudo mv "${TMP}" "${DEST}"
else
    echo "Pas les droits sur ${INSTALL_DIR} et pas de sudo." >&2
    echo "Réessaye avec INSTALL_DIR=~/.local/bin :" >&2
    echo "  curl -sSf https://raw.githubusercontent.com/${REPO}/main/install.sh | INSTALL_DIR=~/.local/bin sh" >&2
    exit 1
fi

# Empêcher le trap de supprimer un fichier maintenant déplacé
trap - EXIT INT TERM

echo "✓ control installé."
echo
"${DEST}" --version || true
echo
echo "Lance \`control --help\` pour commencer."
