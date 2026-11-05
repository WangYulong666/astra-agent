# syntax=docker/dockerfile:1.7
# Astra 沙箱内 CLI agent —— DeepSeek Harness (dsh) 裁剪镜像
#
# 实测（linux/x64）：826MB 全量 → 138MB 落盘 → 25MB 压缩层
# 裁剪后能力验证：initialize / session/new 通过，prompt 仅因无 API key 停止
#
# 两个必须遵守的约束（都是踩过的坑）：
#   1. npm install 必须「一次性装完」且带 --os/--cpu。分批 npm i 任何单包都会按
#      当前构建平台重算依赖树，把 darwin/win32 平台包重新拉回来（+125MB）。
#   2. zod 用 overrides 钉死 4.4.3。46 个 dsh 插件对 zod 是精确版本 4.4.3，
#      npm 默认给每个插件嵌套装一份（46×7MB=322MB）。4.4.3 满足全部 6 处 range 声明。

FROM node:22-bookworm-slim AS build
WORKDIR /opt/dsh
ENV NPM_CONFIG_UPDATE_NOTIFIER=false \
    NPM_CONFIG_FUND=false

COPY package.json ./
RUN npm install --os=linux --cpu=x64 --ignore-scripts --no-audit

COPY trim.sh trim-patch.yml ./
RUN bash trim.sh /opt/dsh/node_modules

# ---------------------------------------------------------------- 运行镜像
FROM node:22-bookworm-slim
WORKDIR /opt/dsh
ENV NODE_ENV=production \
    DSH_HOME=/var/lib/dsh \
    NO_COLOR=1

COPY --from=build /opt/dsh/node_modules ./node_modules
COPY --from=build /opt/dsh/package.json ./package.json
COPY trim-patch.yml ./
COPY verify-acp.mjs ./

# ── 许可合规 ──────────────────────────────────────────────────────────────
# MIT 要求「本许可声明应包含在软件的所有副本或实质性部分中」。镜像里的
# node_modules 是 dsh 的实质部分，所以必须把上游 MIT 全文带进镜像。
#
# 为什么用 heredoc 而不是 COPY 一个 LICENSE 文件：
#   本文件所在目录（dsh-trim/）就是构建上下文，third_party/deepseek-harness/LICENSE
#   在上下文之外，COPY ../third_party/... 会被 Docker 直接拒绝；
#   ADD https://raw.githubusercontent.com/... 要求构建期能出网（内网/CI 不保证）。
#   heredoc 既不吃上下文也不吃网络，唯一代价是文本要跟着上游手工同步——
#   上游 LICENSE 只在换版本时才变，改这里时同步核对 third_party/deepseek-harness/LICENSE。
COPY --chown=node:node <<'THIRD_PARTY_NOTICES_EOF' /opt/dsh/THIRD_PARTY_NOTICES
astra-agent —— 沙箱内 dsh 运行镜像的第三方声明
================================================================================

本镜像包含 DeepSeek Harness (dsh) 及其 npm 依赖，均为第三方软件。
完整许可见 /opt/dsh/node_modules/<package>/LICENSE；
依赖级的第三方声明见 /opt/dsh/node_modules/@deepseek-ai/dsh/THIRD_PARTY_NOTICES.md（若上游打包）。

--------------------------------------------------------------------------------
DeepSeek Harness —— MIT License
https://github.com/deepseek-ai/deepseek-harness
--------------------------------------------------------------------------------

MIT License

Copyright (c) 2026 DeepSeek

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
THIRD_PARTY_NOTICES_EOF

# dsh 会往 DSH_HOME 写 session JSONL / sqlite，沙箱内用可写层挂载
RUN mkdir -p /var/lib/dsh && chown -R node:node /var/lib/dsh /opt/dsh

USER node
# ACP 走 stdio，容器不监听端口；proxy 在宿主侧连 stdin/stdout
ENTRYPOINT ["node", "/opt/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js", \
            "--profile", "acp", "--patch", "/opt/dsh/trim-patch.yml"]
