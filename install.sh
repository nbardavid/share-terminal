#!/bin/sh
# install.sh — installe `control` depuis la dernière GitHub Release.
#
# Usage :
#   curl -sSf https://raw.githubusercontent.com/nbardavid/share-terminal/main/install.sh | sh
#
# Par défaut, installe dans ~/.local/bin (pas de sudo). Si ce répertoire
# n'est pas dans ton PATH, le script te dit quoi ajouter à ton shell rc.
#
# Variables d'environnement supportées :
#   INSTALL_DIR=/path    # défaut : ~/.local/bin
#   VERSION=vX.Y.Z       # défaut : dernière release ("latest")

set -eu

REPO="nbardavid/share-terminal"
BIN="control"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"
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

# --- vérification SHA-256 ---
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
            echo "Checksum SHA-256 incorrect ! Fichier corrompu ou compromis." >&2
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

# --- installation (pas de sudo : on écrit dans le home) ---
mkdir -p "${INSTALL_DIR}"
DEST="${INSTALL_DIR}/${BIN}"
mv "${TMP}" "${DEST}"
trap - EXIT INT TERM

echo "✓ Installé : ${DEST}"

# --- check PATH et conseille en fonction du shell ---
case ":${PATH}:" in
    *":${INSTALL_DIR}:"*)
        echo
        "${DEST}" --version
        echo
        echo "Prêt. Tape \`control --help\` pour commencer."
        ;;
    *)
        echo
        echo "⚠  ${INSTALL_DIR} n'est pas dans ton \$PATH."
        echo "   Ajoute cette ligne à ton fichier de config shell :"
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
                echo "   (à ajouter au fichier rc de ${SHELL_NAME})"
                ;;
        esac
        echo
        echo "   Ou lance le binaire directement : ${DEST} --help"
        ;;
esac
