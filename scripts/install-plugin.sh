#!/usr/bin/env bash
# Installs (or upgrades) fleeting-plugin-upcloud from GitLab releases.
#
# Environment:
#   VERSION                 release tag to install, e.g. v1.3.0 (default: latest)
#   INSTALL_PATH            where to install (default: /usr/local/bin/fleeting-plugin-upcloud)
#   SKIP_CHECKSUM_VERIFY=1  install even if the release publishes no checksum
#
# Upgrading in place is safe while GitLab Runner is running: the new binary
# replaces the old one with an atomic rename, and the runner keeps using the
# old plugin process until it is restarted.
set -euo pipefail

REPO="kirbo/gitlab-fleeting-plugin-upcloud"
# Named fleeting-plugin-* so GitLab Runner discovers it via exec.LookPath("fleeting-plugin-upcloud").
# The plugin field in config.toml must match: plugin = "fleeting-plugin-upcloud"
BINARY_NAME="fleeting-plugin-upcloud"
# Full path the binary is installed to. /usr/local/bin is on root's PATH, so
# config.toml can reference the plugin by bare name. Override with INSTALL_PATH.
INSTALL_PATH="${INSTALL_PATH:-/usr/local/bin/${BINARY_NAME}}"
INSTALL_DIR="$(dirname "${INSTALL_PATH}")"
VERSION="${VERSION:-}"

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

# --- Use sudo only when the install directory (or, if it doesn't exist yet,
# its nearest existing parent) isn't writable ---
WRITE_CHECK_DIR="${INSTALL_DIR}"
while [[ ! -d "${WRITE_CHECK_DIR}" ]]; do
  WRITE_CHECK_DIR="$(dirname "${WRITE_CHECK_DIR}")"
done
SUDO=""
if [[ "$(id -u)" -ne 0 && ! -w "${WRITE_CHECK_DIR}" ]]; then
  if ! command -v sudo &>/dev/null; then
    echo "Error: ${INSTALL_DIR} is not writable and sudo is not available; re-run as root or set INSTALL_PATH." >&2
    exit 1
  fi
  SUDO="sudo"
fi

ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}"
ENCODED_REPO="${REPO//\//%2F}"
if [[ -n "${VERSION}" ]]; then
  API_URL="https://gitlab.com/api/v4/projects/${ENCODED_REPO}/releases/${VERSION}"
else
  API_URL="https://gitlab.com/api/v4/projects/${ENCODED_REPO}/releases/permalink/latest"
fi

echo "Detected platform: ${OS}/${ARCH}"
echo "Fetching ${VERSION:-latest} release info from GitLab..."

RELEASE_JSON="$(curl -fsSL --retry 4 --retry-delay 5 "${API_URL}")"

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
  echo "Error: could not find asset '${ASSET_NAME}' in release ${TAG}." >&2
  echo "Available assets:" >&2
  if command -v jq &>/dev/null; then
    echo "${RELEASE_JSON}" | jq -r '.assets.links[].name' >&2
  fi
  exit 1
fi

echo "Downloading ${ASSET_NAME} (${TAG})..."

TMPFILE="$(mktemp)"
trap 'rm -f "${TMPFILE}"' EXIT

curl -fsSL --retry 4 --retry-delay 5 --progress-bar -o "${TMPFILE}" "${DOWNLOAD_URL}"

# --- Verify checksum (releases publish <asset>.sha256 alongside each binary) ---
if [[ -n "${CHECKSUM_URL}" ]]; then
  echo "Verifying checksum..."
  EXPECTED="$(curl -fsSL --retry 4 --retry-delay 5 "${CHECKSUM_URL}" | awk '{print $1}')"
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
elif [[ "${SKIP_CHECKSUM_VERIFY:-}" == "1" ]]; then
  echo "Warning: no checksum published for ${ASSET_NAME} (${TAG}); installing unverified (SKIP_CHECKSUM_VERIFY=1)." >&2
else
  echo "Error: no checksum published for ${ASSET_NAME} (${TAG}). Set SKIP_CHECKSUM_VERIFY=1 to install anyway." >&2
  exit 1
fi

PREVIOUS=""
if [[ -x "${INSTALL_PATH}" ]]; then
  PREVIOUS="$("${INSTALL_PATH}" version 2>/dev/null | awk '/^Version:/ {print $2}' || true)"
fi

# Copy next to the target, then rename over it: a rename is atomic, so a
# running runner never sees a half-written binary, and replacing a binary
# that is currently executing doesn't fail with "text file busy".
${SUDO} mkdir -p "${INSTALL_DIR}"
${SUDO} install -m 0755 "${TMPFILE}" "${INSTALL_PATH}.new"
${SUDO} mv -f "${INSTALL_PATH}.new" "${INSTALL_PATH}"

INSTALLED="$("${INSTALL_PATH}" version 2>/dev/null | awk '/^Version:/ {print $2}' || true)"
if [[ -n "${PREVIOUS}" ]]; then
  echo "Upgraded: ${INSTALL_PATH} (${PREVIOUS} -> ${INSTALLED:-${TAG}})"
  echo
  echo "GitLab Runner keeps using the old plugin until it restarts. Restart it"
  echo "gracefully (see \"Upgrading\" in the README) to load the new version."
else
  echo "Installed: ${INSTALL_PATH} (${INSTALLED:-${TAG}})"
  echo
  echo "Reference it in /etc/gitlab-runner/config.toml:"
  echo "  [runners.autoscaler]"
  echo "    plugin = \"${BINARY_NAME}\""
  echo
  echo "Verify discovery with: sudo gitlab-runner fleeting list"
fi
