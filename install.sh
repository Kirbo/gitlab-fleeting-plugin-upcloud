#!/usr/bin/env bash
set -euo pipefail

REPO="kirbo/gitlab-fleeting-plugin-upcloud"
BINARY_NAME="fleeting-plugin-upcloud"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.gitlab-runner/plugins}"

# --- Detect OS ---
OS="$(uname -s)"
case "$OS" in
  Linux*)  OS="linux"  ;;
  Darwin*) OS="darwin" ;;
  *)
    echo "Unsupported OS: $OS" >&2
    exit 1
    ;;
esac

# --- Detect architecture ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)        ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}"
ENCODED_REPO="${REPO//\//%2F}"
API_URL="https://gitlab.com/api/v4/projects/${ENCODED_REPO}/releases/permalink/latest"

echo "Detected platform: ${OS}/${ARCH}"
echo "Fetching latest release info from GitLab..."

RELEASE_JSON="$(curl -fsSL "$API_URL")"

# Extract the tag name for display
if command -v jq &>/dev/null; then
  TAG="$(echo "$RELEASE_JSON" | jq -r '.tag_name')"
  DOWNLOAD_URL="$(echo "$RELEASE_JSON" | jq -r --arg name "$ASSET_NAME" \
    '.assets.links[] | select(.name == $name) | .direct_asset_url // .url' | head -1)"
else
  TAG="$(echo "$RELEASE_JSON" | grep -o '"tag_name":"[^"]*"' | head -1 | cut -d'"' -f4)"
  DOWNLOAD_URL="$(echo "$RELEASE_JSON" \
    | grep -o "\"direct_asset_url\":\"[^\"]*${ASSET_NAME}[^\"]*\"" \
    | head -1 | cut -d'"' -f4)"
  # fallback: try 'url' field
  if [[ -z "$DOWNLOAD_URL" ]]; then
    DOWNLOAD_URL="$(echo "$RELEASE_JSON" \
      | grep -o "\"url\":\"[^\"]*${ASSET_NAME}[^\"]*\"" \
      | head -1 | cut -d'"' -f4)"
  fi
fi

if [[ -z "$DOWNLOAD_URL" ]]; then
  echo "Error: could not find asset '${ASSET_NAME}' in the latest release (${TAG})." >&2
  echo "Available assets:" >&2
  if command -v jq &>/dev/null; then
    echo "$RELEASE_JSON" | jq -r '.assets.links[].name' >&2
  fi
  exit 1
fi

echo "Downloading ${ASSET_NAME} (${TAG})..."

TMPFILE="$(mktemp)"
trap 'rm -f "$TMPFILE"' EXIT

curl -fsSL --progress-bar -o "$TMPFILE" "$DOWNLOAD_URL"
chmod +x "$TMPFILE"

mkdir -p "$INSTALL_DIR"
mv "$TMPFILE" "${INSTALL_DIR}/${BINARY_NAME}"

echo "Installed: ${INSTALL_DIR}/${BINARY_NAME}"
