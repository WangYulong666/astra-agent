#!/usr/bin/env bash
# GitOps 发布：把镜像 tag 写进 git，让 ArgoCD 去同步
#
# 为什么 CI 不直接 kubectl apply：
#   直连集群的话，"线上跑的是哪个版本"只能靠 kubectl describe 反查，
#   回滚要"再跑一次流水线"（重新构建、结果可能不一样）。
#   写进 git 之后，每个线上版本都对得上一个 commit，回滚就是 git revert。
set -euo pipefail

TAG="${1:?用法: gitops-bump.sh <tag> [env]}"
ENV="${2:-staging}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVERLAY="$ROOT/k8s/overlays/$ENV/kustomization.yaml"

[ -f "$OVERLAY" ] || { echo "找不到 $OVERLAY"; exit 1; }

# 只改 newTag，不动 newName
# macOS 的 sed 需要 -i ''，Linux 需要 -i，用 perl 统一
perl -pi -e "s/newTag: .*/newTag: ${TAG}/" "$OVERLAY"

if ! git -C "$ROOT" diff --quiet -- "$OVERLAY"; then
  git -C "$ROOT" add "$OVERLAY"
  git -C "$ROOT" commit -m "release(${ENV}): bump images to ${TAG}"
  echo "已提交 tag=${TAG} 到 ${ENV}"
  echo "→ ArgoCD 检测到变更后会自动同步（或手动: argocd app sync astra-${ENV}）"
else
  echo "tag 未变化（已经是 ${TAG}），跳过提交"
fi
