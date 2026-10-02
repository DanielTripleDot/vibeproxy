#!/bin/bash
set -euo pipefail
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_DIR="$PROJECT_DIR/src/Sources/Resources/Plugins"
mkdir -p "$PLUGIN_DIR"
cd "$PROJECT_DIR/routing-plugin"
case "${TARGET_ARCH:-$(uname -m)}" in
    arm64) export GOARCH=arm64 ;;
    x86_64) export GOARCH=amd64 ;;
    *) echo "Unsupported routing plugin architecture" >&2; exit 1 ;;
esac
export CGO_ENABLED=1
export MACOSX_DEPLOYMENT_TARGET=13.0
export CGO_CFLAGS="${CGO_CFLAGS:-} -arch ${TARGET_ARCH:-$(uname -m)}"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -arch ${TARGET_ARCH:-$(uname -m)}"
go build -trimpath -buildmode=c-shared -o "$PLUGIN_DIR/vibeproxy-quota.dylib" .
# Go emits a header alongside the library; it is not an app resource.
rm -f "$PLUGIN_DIR/vibeproxy-quota.h"
