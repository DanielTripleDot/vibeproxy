#!/bin/bash
set -euo pipefail
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT_DIR="$PROJECT_DIR/src/Sources/Resources/Tools"
mkdir -p "$OUTPUT_DIR"
cd "$PROJECT_DIR/routing-plugin"
case "${TARGET_ARCH:-$(uname -m)}" in
    arm64) export GOARCH=arm64 ;;
    x86_64) export GOARCH=amd64 ;;
    *) echo "Unsupported usage MCP architecture" >&2; exit 1 ;;
esac
CGO_ENABLED=0 go build -tags usage_mcp -trimpath -o "$OUTPUT_DIR/vibeproxy-usage-mcp" .
