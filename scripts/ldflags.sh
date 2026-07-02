#!/usr/bin/env bash
# Prints the -ldflags value used by the mise build tasks, embedding version
# metadata from git (falls back to dev/unknown outside a git checkout).
set -euo pipefail

version="$(git describe --tags --always --dirty 2>/dev/null || echo "dev")"
commit="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "-X main.buildVersion=${version} -X main.buildCommit=${commit} -X main.buildDate=${date}"
