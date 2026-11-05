#!/usr/bin/env bash
# 部署后冒烟：不验证业务逻辑，只验证「这套编排真的通了」
#
# 检查顺序刻意从里往外：先看 pod 起没起，再看服务通不通，最后看网关转发对不对。
# 反过来的话网关 502 会把你引向"网关配置错了"，实际是 pod 还在拉镜像。
set -uo pipefail

NS="${NS:-astra-system}"
HOST="${HOST:-api.astra.example.com}"
FAIL=0
ok()   { printf '  \033[32m✅\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m❌\033[0m %s\n' "$*"; FAIL=1; }
warn() { printf '  \033[33m⚠️\033[0m %s\n' "$*"; }

echo "▸ 1. 工作负载就绪"
for d in astra-api astra-worker astra-sandboxd astra-gateway; do
  if kubectl -n "$NS" rollout status "deploy/$d" --timeout=120s >/dev/null 2>&1; then
    ok "$d"
  else
    bad "$d 未就绪"
    kubectl -n "$NS" get pods -l app="$d" -o wide 2>/dev/null | tail -5
  fi
done

echo "▸ 2. 集群内健康检查（绕过网关）"
kubectl -n "$NS" run smoke-$$ --rm -i --restart=Never --image=curlimages/curl:latest --quiet 2>/dev/null \
  -- curl -fsS --max-time 5 http://astra-api:8080/healthz >/dev/null \
  && ok "astra-api /healthz" || bad "astra-api /healthz 不通"

echo "▸ 3. 经 APISIX 的健康检查"
if curl -fsS --max-time 10 "http://${HOST}/v1/healthz" >/dev/null 2>&1; then
  ok "网关 → api 转发正常"
else
  warn "网关转发失败（若网关未部署或 DNS 未配可忽略）"
fi

echo "▸ 4. SSE 是否真流式（最容易踩的坑）"
# 判据：如果网关开了 buffering，curl 会一直等到连接结束才吐出内容。
# 这里用 --max-time 3 主动切断，看是否已经收到数据；收到字节 > 0 说明是流式的。
SSE_BYTES=$(curl -sN --max-time 3 "http://${HOST}/v1/runs/smoke-test/events" 2>/dev/null | head -c 200 | wc -c | tr -d ' ')
if [ "${SSE_BYTES:-0}" -gt 0 ]; then
  ok "SSE 客户端在 3s 内收到 ${SSE_BYTES} 字节（流式正常）"
else
  warn "SSE 3s 内无数据 —— 若路由未开 proxy-control 关 buffering，这就是症状"
fi

echo "▸ 5. 沙箱链路"
if kubectl -n astra-cube get pods -l app=cube-node --no-headers 2>/dev/null | grep -q Running; then
  ok "cube-node DaemonSet 有 Running 实例"
else
  warn "cube 池未就绪（M0/M1 阶段可能还没部署）"
fi

echo
if [ "$FAIL" = "0" ]; then
  printf '\033[32m冒烟通过\033[0m\n'
else
  printf '\033[31m冒烟失败，先修上面 ❌ 的项\033[0m\n'
fi
exit "$FAIL"
