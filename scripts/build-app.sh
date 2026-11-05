#!/bin/bash
# 组装 Astra.app 并产出 dmg。
#
# 不用 wails build：前端已在 web/ 由 Vite 构建并内嵌进 Go 二进制，
# 壳只是 WKWebView，bundle 结构手写比引入第三套工程模板可靠。
# ⚠ macOS 链接必须带 CGO_LDFLAGS（缺 Xcode 时 UTType 符号缺失），别去掉。
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-0.2.0}"
BUILD="${BUILD:-$(date +%Y%m%d%H%M)}"
OUT="dist"
APP="$OUT/Astra.app"

rm -rf "$APP" "$OUT/Astra-*.dmg"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "▸ 编译二进制（arm64）"
CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
  go build -tags "desktop,production" \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$APP/Contents/MacOS/Astra" ./cmd/astra-desktop

echo "▸ 组装 bundle"
sed -e "s/__VERSION__/$VERSION/" -e "s/__BUILD__/$BUILD/" desktop/Info.plist \
  > "$APP/Contents/Info.plist"
cp desktop/astra.icns "$APP/Contents/Resources/Astra.icns"
# PkgInfo: APPL + 签名占位
printf 'APPL????' > "$APP/Contents/PkgInfo"

# 辅助二进制必须跟主程序同目录（SiblingBinary 按同目录解析）。
# 缺了它们，App 能开但「起会话」直接失败——这是「能开 ≠ 能用」的经典坑。
echo "▸ 拷贝辅助二进制（acp-proxy / acp-shell-agent）"
go build -o "$APP/Contents/MacOS/acp-proxy" ./cmd/acp-proxy
go build -o "$APP/Contents/MacOS/acp-shell-agent" ./cmd/acp-shell-agent

# 可选：WITH_DSH=1 把本机 dsh 打进 bundle（+140MB）。换了机器没有 home 副本时，
# 桌面端会探测到 bundle 内副本并用 ASTRA_AGENT 指过去；都没有则降级为 shell。
if [[ "${WITH_DSH:-}" == "1" ]]; then
  DSH_SRC="$HOME/.astra/agent-dsh"
  if [[ -f "$DSH_SRC/node_modules/@deepseek-ai/dsh/lib/bin.js" ]]; then
    echo "▸ 拷贝 dsh 进 bundle Resources"
    cp -R "$DSH_SRC" "$APP/Contents/Resources/agent-dsh"
  else
    echo "⚠ WITH_DSH=1 但 $DSH_SRC 里没有 dsh（bin.js 不存在），跳过"
  fi
fi

echo "▸ 签名（ad-hoc + hardened runtime；有 Developer ID 时设 IDENTITY 变量可公证）"
if [ -n "${IDENTITY:-}" ]; then
  codesign --force --deep --options runtime --entitlements desktop/entitlements.plist --sign "$IDENTITY" "$APP"
else
  codesign --force --deep --options runtime --entitlements desktop/entitlements.plist --sign - "$APP" \
    || codesign --force --deep --sign - "$APP"
fi

echo "▸ 自检"
plutil -lint "$APP/Contents/Info.plist"
codesign --verify --deep "$APP" && echo "  签名通过"
du -sh "$APP"

echo "▸ 打 dmg"
hdiutil create -volname "Astra" -srcfolder "$APP" -ov -format UDZO \
  "$OUT/Astra-$VERSION.dmg" | tail -1
shasum -a 256 "$OUT/Astra-$VERSION.dmg" | awk '{print "  sha256  " $1}'
ls -lh "$OUT"
