#!/bin/bash
# 跨平台服务器端发布包：linux/amd64、linux/arm64、darwin/arm64、windows/amd64。
#
# 纯 Go（modernc.org/sqlite 无 cgo），CGO_ENABLED=0 在 macOS 本机即可出全部平台。
# 每平台一个压缩包（内含 astra-server / acp-proxy / acp-shell-agent），
# 输出到 dist/release/，最后生成 SHA256SUMS。
#
# 用法：
#   bash scripts/build-release.sh                # 版本取 git describe
#   VERSION=v0.3.0 bash scripts/build-release.sh # 指定版本号
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
OUT="dist/release"
ABS_OUT="$(cd "$(dirname "$0")/.." && pwd)/dist/release"
STAGE="$OUT/.stage"
DIST="internal/api/web/dist"

# bash 3.2 没有 mapfile / 关联数组，全用普通变量。

# ── 前端：dist 需比 web 源码新，否则重新构建 ──────────────────
need_web=1
if [ -f "$DIST/index.html" ] && [ -z "$(find web/src web/index.html web/package.json -newer "$DIST/index.html" -print -quit 2>/dev/null)" ]; then
  need_web=0
fi
if [ "$need_web" = 1 ]; then
  echo "▸ 构建前端（产物内嵌进 astra-server）"
  (cd web && npm install --no-audit --no-fund && npm run build)
else
  echo "▸ 前端 dist 已比源码新，跳过构建"
fi

rm -rf "$OUT"
mkdir -p "$OUT" "$STAGE"

# ── 交叉编译 + 打包 ───────────────────────────────────────────
# 逐平台进行：三个二进制编进 .stage，压完即清，包内是平铺文件名。
for plat in "darwin arm64" "linux amd64" "linux arm64" "windows amd64"; do
  set -- $plat
  goos=$1 goarch=$2
  ext=""
  [ "$goos" = "windows" ] && ext=".exe"

  echo "▸ 编译 $goos/$goarch"
  for target in astra-server acp-proxy acp-shell-agent; do
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -ldflags "-s -w" -o "$STAGE/$target$ext" "./cmd/$target"
  done

  name="astra-$VERSION-$goos-$goarch"
  if [ "$goos" = "windows" ]; then
    # zip 输出必须用绝对路径：STAGE 在 mktemp 下，相对 ../.. 会指错地方
    if command -v zip >/dev/null 2>&1; then
      (cd "$STAGE" && zip -q -r "$ABS_OUT/$name.zip" .)
    else
      # zip 缺失时退回 python3（macOS/Linux 基本都有）
      (cd "$STAGE" && python3 -m zipfile -c "$ABS_OUT/$name.zip" ./*)
    fi
  else
    tar -czf "$OUT/$name.tar.gz" -C "$STAGE" .
  fi
  rm -f "$STAGE"/*
done

rm -rf "$STAGE"

# ── 校验和 ───────────────────────────────────────────────────
if command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD="sha256sum"
else
  SHA_CMD="shasum -a 256"
fi
echo
echo "▸ Windows 桌面端（交叉编译，WebView2 运行时由目标机提供）"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags "desktop,production" \
  -ldflags "-s -w" -o dist/release/Astra-desktop-windows-amd64.exe ./cmd/astra-desktop

echo "▸ SHA256SUMS"
( cd "$OUT" && $SHA_CMD astra-*.tar.gz astra-*.zip Astra-desktop-*.exe > SHA256SUMS )

echo "▸ 发布包（dist/release/）："
ls -lh "$OUT"
