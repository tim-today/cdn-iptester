#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
GOOS_NAME="$(go env GOOS)"
GOARCH_NAME="$(go env GOARCH)"
EXT=""

if [[ "$GOOS_NAME" == "windows" ]]; then
  EXT=".exe"
fi

mkdir -p "$DIST_DIR"
mkdir -p "$DIST_DIR/configs"
cp "$ROOT_DIR/configs/config.example.yaml" "$DIST_DIR/configs/config.example.yaml"
if [[ -f "$ROOT_DIR/configs/config.yaml" ]]; then
  cp "$ROOT_DIR/configs/config.yaml" "$DIST_DIR/configs/config.yaml"
fi

if command -v wails >/dev/null 2>&1; then
  echo "building desktop bundle with wails"
  wails build
  exit 0
fi

echo "building $GOOS_NAME/$GOARCH_NAME"
go build -trimpath -ldflags="-s -w" -o "$DIST_DIR/cdn-iptester-$GOOS_NAME-$GOARCH_NAME$EXT" "$ROOT_DIR/cmd/cdn-iptester"
