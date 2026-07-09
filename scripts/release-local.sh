#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_DIR="$ROOT_DIR/release"
VERSION="${1:-}"
UPLOAD_FLAG="${2:-}"
REPO="${REPO:-}"
ASSETS=()

if [[ -z "$VERSION" ]]; then
  echo "用法: ./scripts/release-local.sh <tag> [--upload]"
  echo "示例: ./scripts/release-local.sh v1.0.1"
  echo "示例: ./scripts/release-local.sh v1.0.1 --upload"
  exit 1
fi

if ! command -v wails >/dev/null 2>&1; then
  echo "未检测到 wails，请先安装："
  echo "go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0"
  exit 1
fi

mkdir -p "$RELEASE_DIR"

GOOS_NAME="$(go env GOOS)"
GOARCH_NAME="$(go env GOARCH)"

resolve_repo() {
  if [[ -n "$REPO" ]]; then
    return 0
  fi
  local remote_url=""
  remote_url="$(git -C "$ROOT_DIR" remote get-url origin 2>/dev/null || true)"
  if [[ "$remote_url" =~ github\.com[:/]([^/]+/[^/.]+)(\.git)?$ ]]; then
    REPO="${BASH_REMATCH[1]}"
    return 0
  fi
  echo "无法从 origin 自动识别 GitHub 仓库，请手动设置 REPO=owner/name 后重试。"
  exit 1
}

build_macos() {
  local asset_name="$1"
  local platform="$2"
  local app_path=""
  echo "开始构建 $platform ..."
  wails build -clean -platform "$platform"
  app_path="$(find "$ROOT_DIR/build/bin" -maxdepth 1 -type d -name '*.app' | head -n 1)"
  if [[ -z "$app_path" ]]; then
    echo "未找到 macOS 应用产物(.app)，请检查 Wails 构建输出。"
    exit 1
  fi
  rm -f "$RELEASE_DIR/$asset_name"
  ditto -c -k --sequesterRsrc --keepParent "$app_path" "$RELEASE_DIR/$asset_name"
  ASSETS+=("$RELEASE_DIR/$asset_name")
}

build_windows() {
  local asset_name="cdn-iptester-windows-amd64.zip"
  local exe_path=""
  echo "开始构建 windows/amd64 ..."
  wails build -clean -platform windows/amd64 -webview2 download
  exe_path="$(find "$ROOT_DIR/build/bin" -maxdepth 1 -type f -name '*.exe' | head -n 1)"
  if [[ -z "$exe_path" ]]; then
    echo "未找到 Windows 可执行文件(.exe)，请检查 Wails 构建输出。"
    exit 1
  fi
  rm -f "$RELEASE_DIR/$asset_name"
  ditto -c -k "$exe_path" "$RELEASE_DIR/$asset_name"
  ASSETS+=("$RELEASE_DIR/$asset_name")
}

case "$GOOS_NAME/$GOARCH_NAME" in
  darwin/arm64|darwin/amd64)
    build_macos "cdn-iptester-darwin-arm64.zip" "darwin/arm64"
    build_macos "cdn-iptester-darwin-amd64.zip" "darwin/amd64"
    build_windows
    ;;
  windows/amd64)
    echo "当前脚本请在 PowerShell 下运行 scripts/release-local.ps1"
    exit 1
    ;;
  *)
    echo "当前平台 $GOOS_NAME/$GOARCH_NAME 暂未内置完整本地 release 打包流程。"
    echo "推荐在 macOS 上执行本脚本，一次生成 macOS 与 Windows 包。"
    exit 1
    ;;
esac

printf '构建完成：\n'
for asset in "${ASSETS[@]}"; do
  echo "- $asset"
done

if [[ "$UPLOAD_FLAG" != "--upload" ]]; then
  echo "如需上传到 GitHub Release，请追加 --upload"
  exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "未检测到 gh，请先安装 GitHub CLI 后重试上传。"
  exit 1
fi

resolve_repo
echo "上传目标仓库：$REPO"

if ! gh release view "$VERSION" --repo "$REPO" >/dev/null 2>&1; then
  echo "Release $VERSION 不存在，正在创建..."
  gh release create "$VERSION" "${ASSETS[@]}" --repo "$REPO" --title "CDN-IPtester $VERSION" --generate-notes
else
  echo "Release $VERSION 已存在，正在上传/覆盖资产..."
  gh release upload "$VERSION" "${ASSETS[@]}" --repo "$REPO" --clobber
fi

echo "上传完成：$VERSION"
