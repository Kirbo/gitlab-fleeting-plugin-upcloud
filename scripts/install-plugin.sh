#!/usr/bin/env bash
set -euo pipefail

REPO="kirbo/gitlab-fleeting-plugin-upcloud"
# Named fleeting-plugin-* so GitLab Runner discovers it via exec.LookPath("fleeting-plugin-upcloud").
# The plugin field in config.toml must match: plugin = "fleeting-plugin-upcloud"
BINARY_NAME="fleeting-plugin-upcloud"
# Full path the binary is installed to. /usr/local/bin is on root's PATH, so
# config.toml can reference the plugin by bare name. Override with INSTALL_PATH.
INSTALL_PATH="${INSTALL_PATH:-/usr/local/bin/${BINARY_NAME}}"

# --- Detect OS ---
OS="$(uname -s)"
case "${OS}" in
  Linux*)  OS="linux"  ;;
  Darwin*) OS="darwin" ;;
  *)
    echo "Unsupported OS: ${OS}" >&2
    exit 1
    ;;
esac

# --- Detect architecture ---
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64)        ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: ${ARCH}" >&2
    exit 1
    ;;
esac

ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}"
ENCODED_REPO="${REPO//\//%2F}"
API_URL="https://gitlab.com/api/v4/projects/${ENCODED_REPO}/releases/permalink/latest"

echo "Detected platform: ${OS}/${ARCH}"
echo "Fetching latest release info from GitLab..."

RELEASE_JSON="$(curl -fsSL "${API_URL}")"

# Extract the tag name for display
if command -v jq &>/dev/null; then
  TAG="$(echo "${RELEASE_JSON}" | jq -r '.tag_name')"
  DOWNLOAD_URL="$(echo "${RELEASE_JSON}" | jq -r --arg name "${ASSET_NAME}" \
    '.assets.links[] | select(.name == $name) | .direct_asset_url // .url' | head -1)"
  CHECKSUM_URL="$(echo "${RELEASE_JSON}" | jq -r --arg name "${ASSET_NAME}.sha256" \
    '.assets.links[] | select(.name == $name) | .direct_asset_url // .url' | head -1)"
else
  TAG="$(echo "${RELEASE_JSON}" | grep -o '"tag_name":"[^"]*"' | head -1 | cut -d'"' -f4)"
  DOWNLOAD_URL="$(echo "${RELEASE_JSON}" \
    | grep -o "\"direct_asset_url\":\"[^\"]*${ASSET_NAME}\"" \
    | head -1 | cut -d'"' -f4)"
  CHECKSUM_URL="$(echo "${RELEASE_JSON}" \
    | grep -o "\"direct_asset_url\":\"[^\"]*${ASSET_NAME}.sha256\"" \
    | head -1 | cut -d'"' -f4)"
  # fallback: try 'url' field
  if [[ -z "${DOWNLOAD_URL}" ]]; then
    DOWNLOAD_URL="$(echo "${RELEASE_JSON}" \
      | grep -o "\"url\":\"[^\"]*${ASSET_NAME}\"" \
      | head -1 | cut -d'"' -f4)"
  fi
  if [[ -z "${CHECKSUM_URL}" ]]; then
    CHECKSUM_URL="$(echo "${RELEASE_JSON}" \
      | grep -o "\"url\":\"[^\"]*${ASSET_NAME}.sha256\"" \
      | head -1 | cut -d'"' -f4)"
  fi
fi

if [[ -z "${DOWNLOAD_URL}" ]]; then
  echo "Error: could not find asset '${ASSET_NAME}' in the latest release (${TAG})." >&2
  echo "Available assets:" >&2
  if command -v jq &>/dev/null; then
    echo "${RELEASE_JSON}" | jq -r '.assets.links[].name' >&2
  fi
  exit 1
fi

echo "Downloading ${ASSET_NAME} (${TAG})..."

TMPFILE="$(mktemp)"
trap 'rm -f "${TMPFILE}"' EXIT

curl -fsSL --progress-bar -o "${TMPFILE}" "${DOWNLOAD_URL}"

# --- Verify checksum (releases publish <asset>.sha256 alongside each binary) ---
if [[ -n "${CHECKSUM_URL}" ]]; then
  echo "Verifying checksum..."
  EXPECTED="$(curl -fsSL "${CHECKSUM_URL}" | awk '{print $1}')"
  if command -v sha256sum &>/dev/null; then
    ACTUAL="$(sha256sum "${TMPFILE}" | awk '{print $1}')"
  else
    ACTUAL="$(shasum -a 256 "${TMPFILE}" | awk '{print $1}')"
  fi
  if [[ -z "${EXPECTED}" || "${EXPECTED}" != "${ACTUAL}" ]]; then
    echo "Error: checksum mismatch for ${ASSET_NAME} (expected ${EXPECTED:-<none>}, got ${ACTUAL})." >&2
    exit 1
  fi
  echo "Checksum OK."
else
  echo "Warning: no checksum published for ${ASSET_NAME} (${TAG}); skipping verification." >&2
fi

chmod +x "${TMPFILE}"

mkdir -p "$(dirname "${INSTALL_PATH}")"
mv "${TMPFILE}" "${INSTALL_PATH}"
chmod +x "${INSTALL_PATH}"

echo "Installed: ${INSTALL_PATH}"
echo
echo "Reference it in /etc/gitlab-runner/config.toml:"
echo "  [runners.autoscaler]"
echo "    plugin = \"${BINARY_NAME}\""
echo
echo "Verify discovery with: gitlab-runner fleeting list"
