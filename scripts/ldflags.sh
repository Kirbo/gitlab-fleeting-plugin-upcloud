#!/usr/bin/env bash
# Prints the -ldflags value used by the build tasks, embedding version
# metadata. BUILD_VERSION and BUILD_COMMIT override what git reports (CI sets
# them; its image has no git). Falls back to dev/unknown outside a checkout.
set -euo pipefail

version="${BUILD_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}"
commit="${BUILD_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")}"
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "-X main.buildVersion=${version} -X main.buildCommit=${commit} -X main.buildDate=${date}"
