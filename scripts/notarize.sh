#!/bin/bash
# 公证脚本：需要 Apple Developer Program 账号（付费）。
#
# 用法：
#   APPLE_ID=you@example.com APPLE_TEAM_ID=XXXXXXXXXX APPLE_APP_SPECIFIC_PASSWORD=xxxx \
#     ./scripts/notarize.sh dist/Astra-0.2.0.dmg
#
# 前置：用 Developer ID Application 证书重签（ad-hoc 签名不能公证）：
#   codesign --force --deep --options runtime --entitlements desktop/entitlements.plist \
#     --sign "Developer ID Application: <名字> (TEAMID)" dist/Astra.app
#   ./scripts/build-app.sh 产出的包先走这一步，再跑本脚本。
set -euo pipefail
PKG="${1:?用法: notarize.sh <dmg 或 app 路径>}"
: "${APPLE_ID:?需要 APPLE_ID}" "${APPLE_TEAM_ID:?需要 APPLE_TEAM_ID}" "${APPLE_APP_SPECIFIC_PASSWORD:?需要 APPLE_APP_SPECIFIC_PASSWORD（在 appleid.apple.com 生成）}"

echo "▸ 提交公证（notarytool）"
xcrun notarytool submit "$PKG" \
  --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_SPECIFIC_PASSWORD" \
  --wait

echo "▸ 装订票据（仅 .app/.dmg 支持 staple）"
xcrun stapler staple "$PKG" || true

echo "▸ 校验"
spctl -a -vv "$PKG" 2>/dev/null || true
xcrun stapler validate "$PKG" && echo "✓ 公证完成"
