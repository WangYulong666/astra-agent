# syntax=docker/dockerfile:1.7
#
# 沙箱内 agent 镜像 = 裁剪后的 dsh + acp-proxy
#
# 为什么把两者放在一个镜像里：
#   acp-proxy 的职责是把 agent 的 stdio 桥成 WSS。它必须和 agent 在同一个 PID 命名空间里
#   才能拉起子进程——跨容器就得走 exec API，那又回到「控制面持有 pipe」的老问题（见 ADR-0002）。
#
# 构建：
#   docker build -f images/agent/Dockerfile -t astra/agent-dsh:0.2.1-rc.2 .
#
# ⚠️ 尚未在本机验证构建。本机是 macOS arm64 且没装 docker，而这个镜像的目标平台是
#    linux/x64（沙箱节点）。第一次构建请在目标架构上跑，并按 dsh-trim/README.md 复核体积：
#    裁剪后应为「落盘 138MB / 压缩层 25MB」。如果数字对不上，说明 npm 平台过滤又漏了。

# ── 1. 编译 acp-proxy（静态二进制，无 CGO）───────────────────
FROM golang:1.27-bookworm AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/acp-proxy ./cmd/acp-proxy
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/acp-proxy ./cmd/acp-proxy

# ── 2. 构建裁剪后的 dsh ─────────────────────────────────────
# 必须一次性装完并带 --os/--cpu：分批 npm i 会按构建平台重算依赖树，
# 把 darwin/win32 平台包重新拉回来（实测 +125MB）。
FROM node:22-bookworm-slim AS dsh
WORKDIR /opt/dsh
ENV NPM_CONFIG_UPDATE_NOTIFIER=false \
    NPM_CONFIG_FUND=false
COPY dsh-trim/package.json ./
RUN npm install --os=linux --cpu=x64 --ignore-scripts --no-audit
COPY dsh-trim/trim.sh dsh-trim/trim-patch.yml ./
RUN bash trim.sh /opt/dsh/node_modules

# ── 3. 运行时 ───────────────────────────────────────────────
FROM node:22-bookworm-slim

WORKDIR /opt/astra
ENV NODE_ENV=production \
    DSH_HOME=/var/lib/dsh \
    NO_COLOR=1 \
    ASTRA_LISTEN=:8080

COPY --from=dsh /opt/dsh/node_modules /opt/dsh/node_modules
COPY --from=dsh /opt/dsh/package.json /opt/dsh/package.json
COPY --from=dsh /opt/dsh/trim-patch.yml /opt/dsh/trim-patch.yml
COPY --from=gobuild /out/acp-proxy /usr/local/bin/acp-proxy

RUN mkdir -p /var/lib/dsh /workspace \
 && chown -R node:node /var/lib/dsh /workspace /opt/dsh

USER node
EXPOSE 8080

# slim 镜像里没有 curl，用 node 自带的 fetch 探活
HEALTHCHECK --interval=15s --timeout=3s --start-period=25s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:8080/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["/usr/local/bin/acp-proxy", \
            "-listen", ":8080", \
            "-cwd", "/workspace", \
            "-agent", "node /opt/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js --profile acp --patch /opt/dsh/trim-patch.yml"]
