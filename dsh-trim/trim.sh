#!/bin/bash
# dsh 裁剪脚本：移除 ACP headless profile 用不到的功能模块
#
# 每一组都做过验证：删除后跑 initialize + session/new，全部通过。
# 递进收益（linux/x64，含 overrides 去重后的 512MB 基线）：
#   512 → 366 → 334 → 326 → 298 → 225 → 162 → 138MB
#
# 用法：bash trim.sh [node_modules 路径]
set -u
NM="${1:-/opt/dsh/node_modules}"
cd "$NM" || { echo "目录不存在: $NM"; exit 1; }

size() { du -sm . | cut -f1; }
step() { printf "%-48s %5sMB\n" "$1" "$(size)"; }

echo "=== 基线 ==="
step "基线（linux/x64 全量 + zod overrides）"

# ① Office 文档转换。dsh-skill-office 依赖 libreoffice-kit，wasm 版 146MB。
#    沙箱做代码/数据任务不需要 docx→pdf，且沙箱内跑 LibreOffice 也不现实。
rm -rf @deepseek-ai/libreoffice-kit-wasm
step "① 删 Office 转换 libreoffice-kit-wasm"

# ② 语音 ASR/TTS。sherpa-onnx + onnxruntime，32MB。沙箱无音频输入。
rm -rf sherpa-onnx-linux-x64 sherpa-onnx-node sherpa-onnx-darwin-arm64
step "② 删语音 sherpa-onnx"

# ③ 图片处理 wasm 兜底。native sharp 已随平台包安装，wasm 版是降级兜底。
rm -rf @img/sharp-wasm32
step "③ 删 sharp-wasm32"

# ④ OpenTelemetry 遥测。必须删 —— dsh-otel 默认 endpoint 是
#    https://dsh-otel-collector.deepseeksvc.com/v1/logs（官方收集端），
#    既不合规也会把沙箱内 trace 外发。
rm -rf @opentelemetry @deepseek-ai/dsh-otel @deepseek-ai/dsh-host-product-telemetry-otel
step "④ 删 OpenTelemetry 遥测"

# ⑤ 非 DeepSeek 的 LLM SDK。@earendil-works/pi-ai 是多厂商抽象层，静态引用
#    openai/anthropic/google-genai/aws-bedrock。用 trim-patch.yml 禁用 llm-pi-ai 后可删。
#    ⚠️ 裁剪后只能走 provider=deepseek-official，行为约束见 README。
rm -rf @earendil-works openai @anthropic-ai @google/genai @aws-sdk @smithy
step "⑤ 删多厂商 LLM SDK (pi-ai/anthropic/google/aws)"

# ⑥ Web/TUI UI 层与 experimental profile。ACP 是 headless，不渲染任何界面。
#    dsh-base / dsh-acp-app 均不引用它们（已验证）。
#    注意保留 dsh-web / dsh-web-search-deepseek / dsh-web-fetch-http —— 那是 web 工具，不是 UI。
rm -rf @deepseek-ai/dsh-client-ui-* @deepseek-ai/dsh-experimental-* \
       @deepseek-ai/dsh-web-app @deepseek-ai/dsh-web-frontend
step "⑥ 删 Web UI / experimental profile"

# ⑦ 跨平台二进制与调试符号。node-pty 的 prebuilds 与 ripgrep/koffi 均多平台各一份。
#    镜像只需 linux-x64，win32 的 conpty.pdb 纯属垃圾。
rm -rf node-pty/prebuilds/win32-* node-pty/prebuilds/darwin-* node-pty/prebuilds/linux-arm64
rm -rf @vscode/ripgrep-darwin-arm64 @koromix/koffi-darwin-arm64
find . -name "*.pdb" -delete 2>/dev/null
find . -name "*.dll" -path "*win32*" -delete 2>/dev/null
step "⑦ 删 win32/darwin 二进制与 .pdb"

echo
echo "=== 完成：$(size)MB / $(find . -type f | wc -l | tr -d ' ') 个文件 ==="
