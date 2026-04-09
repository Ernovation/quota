#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${1:-linux-amd64}"
OUTPUT_DIR="${2:-$SCRIPT_DIR/dist}"

if ! command -v go &>/dev/null; then
    echo "Error: Go executable not found in PATH. Install Go and ensure it is available." >&2
    exit 1
fi

mkdir -p "$OUTPUT_DIR/$TARGET"

OUTPUT_NAME="quota-server"
GOOS=""
GOARCH=""
GOMIPS=""
GOARM=""
CGO_ENABLED="0"

case "$TARGET" in
    windows)
        GOOS="windows"
        GOARCH="amd64"
        CGO_ENABLED=""
        OUTPUT_NAME="quota-server.exe"
        ;;
    linux-amd64)
        GOOS="linux"
        GOARCH="amd64"
        ;;
    openwrt-mips)
        GOOS="linux"
        GOARCH="mips"
        GOMIPS="softfloat"
        ;;
    openwrt-mipsle)
        GOOS="linux"
        GOARCH="mipsle"
        GOMIPS="softfloat"
        ;;
    openwrt-arm64)
        GOOS="linux"
        GOARCH="arm64"
        ;;
    openwrt-armv7)
        GOOS="linux"
        GOARCH="arm"
        GOARM="7"
        ;;
    *)
        echo "Error: Unsupported target: $TARGET" >&2
        echo "Valid targets: windows, linux-amd64, openwrt-mips, openwrt-mipsle, openwrt-arm64, openwrt-armv7" >&2
        exit 1
        ;;
esac

OUT_PATH="$OUTPUT_DIR/$TARGET/$OUTPUT_NAME"

cd "$SCRIPT_DIR"

go mod tidy

env GOOS="$GOOS" \
    GOARCH="$GOARCH" \
    ${GOMIPS:+GOMIPS="$GOMIPS"} \
    ${GOARM:+GOARM="$GOARM"} \
    ${CGO_ENABLED:+CGO_ENABLED="$CGO_ENABLED"} \
    go build -trimpath -ldflags "-s -w" -o "$OUT_PATH" ./cmd/server

echo "Built $TARGET binary: $OUT_PATH"
