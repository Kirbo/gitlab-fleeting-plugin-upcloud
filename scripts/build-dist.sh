#!/usr/bin/env bash
# Cross-compiles the release binaries into dist/ under their release asset
# names (fleeting-plugin-upcloud-<os>-<arch>[.exe]), each with a .sha256 file.
# Used by `mise run dist` and the CI build job.
#
# Environment:
#   BUILD_VERSION, BUILD_COMMIT  version metadata (default: from git; see ldflags.sh)
#   TARGETS                      space-separated GOOS/GOARCH pairs (default: all release targets)
#   DIST                         output directory (default: dist)
set -euo pipefail

BINARY_NAME="fleeting-plugin-upcloud"
DIST="${DIST:-dist}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
LDFLAGS="$("$(dirname "$0")/ldflags.sh")"

sha256() {
  if command -v sha256sum &>/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

rm -rf "${DIST}"
mkdir -p "${DIST}"

for target in ${TARGETS}; do
  os="${target%/*}"
  arch="${target#*/}"
  ext=""
  if [[ "${os}" == "windows" ]]; then ext=".exe"; fi
  asset="${BINARY_NAME}-${os}-${arch}${ext}"

  echo "Building ${asset}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${DIST}/${asset}" .
  (cd "${DIST}" && sha256 "${asset}" > "${asset}.sha256")
done
