#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist/linux-x64"
GOCACHE_DIR="$ROOT_DIR/.gocache"
APP_NAME="pr-agent-go"
ARCHIVE_NAME="${APP_NAME}-linux-x64.tar.gz"

mkdir -p "$DIST_DIR" "$GOCACHE_DIR"
rm -f "$DIST_DIR/$APP_NAME" "$DIST_DIR/$ARCHIVE_NAME" "$DIST_DIR/SHA256SUMS"

echo "[build] running tests"
(
  cd "$ROOT_DIR"
  GOCACHE="$GOCACHE_DIR" go test ./...
)

echo "[build] compiling linux x86-64 binary"
(
  cd "$ROOT_DIR"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOCACHE="$GOCACHE_DIR" \
    go build -trimpath -ldflags="-s -w" -o "$DIST_DIR/$APP_NAME" ./cmd/server
)

cp "$ROOT_DIR/.env.example" "$DIST_DIR/.env.example"
cp "$ROOT_DIR/README.md" "$DIST_DIR/README.md"

chmod +x "$DIST_DIR/$APP_NAME"

echo "[build] generating checksums"
(
  cd "$DIST_DIR"
  shasum -a 256 "$APP_NAME" ".env.example" "README.md" > SHA256SUMS
  tar -czf "$ARCHIVE_NAME" "$APP_NAME" ".env.example" "README.md" "SHA256SUMS"
)

echo "[build] done"
echo "[build] binary: $DIST_DIR/$APP_NAME"
echo "[build] archive: $DIST_DIR/$ARCHIVE_NAME"
