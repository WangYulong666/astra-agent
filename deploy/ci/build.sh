#!/usr/bin/env bash
# Astra 构建流水线（可移植：CNB / GitLab CI / Gitea Actions 都直接调这个脚本）
#
# 为什么核心逻辑放在 shell 而不是 CI 平台 DSL：
#   M4 阶段要从 CNB（云托管）切到 Gitea + Woodpecker（自部署，守源码可控红线）。
#   把构建逻辑写成普通 shell，切换时只换流水线壳子，不用重写。
#
# 用法：
#   ./build.sh                      # 构建全部镜像，不推送
#   REGISTRY=harbor.astra.local/astra TAG=abc1234 PUSH=1 ./build.sh
set -euo pipefail

REGISTRY="${REGISTRY:-harbor.astra.local/astra}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
PUSH="${PUSH:-0}"
BINS=(api worker sandboxd gateway)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

info() { printf '\n\033[1;36m▸ %s\033[0m\n' "$*"; }

# ── 1. 静态检查与单测 ────────────────────────────────────────
info "lint + test"
(cd backend && go vet ./... && go test ./... -count=1 -race)

if [ -d web ] && [ -f web/package.json ]; then
  info "前端 lint + build"
  (cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm build)
  # SPA 产物 embed 进 astra-api：不单独起 nginx，少一个 Deployment、
  # 少一套「静态资源版本 ≠ API 版本」的错配
  info "前端产物拷入 backend 供 go:embed"
  rm -rf backend/internal/api/webdist
  cp -R web/dist backend/internal/api/webdist
fi

# ── 2. 构建镜像 ──────────────────────────────────────────────
for b in "${BINS[@]}"; do
  info "build image: astra-${b}:${TAG}"
  docker build \
    -f deploy/ci/Dockerfile \
    --build-arg BIN="${b}" \
    --build-arg VERSION="${TAG}" \
    --build-arg COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    -t "${REGISTRY}/astra-${b}:${TAG}" \
    -t "${REGISTRY}/astra-${b}:latest" \
    backend
done

# ── 3. 镜像大小自检 ──────────────────────────────────────────
# distroless + 静态二进制的正常区间是 15–30MB。超了说明有东西漏进镜像
# （最常见是 CGO 没关掉、或者把整个源码目录拷进去了）。
info "镜像大小"
for b in "${BINS[@]}"; do
  sz=$(docker image inspect "${REGISTRY}/astra-${b}:${TAG}" --format '{{.Size}}')
  mb=$(( sz / 1024 / 1024 ))
  printf '  astra-%-10s %4dMB' "$b" "$mb"
  if [ "$mb" -gt 60 ]; then printf '  \033[1;33m⚠️ 偏大\033[0m'; fi
  printf '\n'
done

# ── 4. 推送 ─────────────────────────────────────────────────
if [ "$PUSH" = "1" ]; then
  info "push to ${REGISTRY}"
  for b in "${BINS[@]}"; do
    docker push "${REGISTRY}/astra-${b}:${TAG}"
  done

  # 可选：签名（cosign，Apache 2.0）。开了就必须在 ArgoCD 侧配验证策略，
  # 否则签名只是摆设。
  if command -v cosign >/dev/null 2>&1 && [ -n "${COSIGN_KEY:-}" ]; then
    info "cosign sign"
    for b in "${BINS[@]}"; do
      cosign sign --key "$COSIGN_KEY" --yes "${REGISTRY}/astra-${b}:${TAG}"
    done
  fi
fi

info "完成：${REGISTRY}/astra-*:${TAG}"
echo "$TAG" > /tmp/astra-image-tag
