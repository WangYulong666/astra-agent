#!/bin/bash
# astra-agent 端到端冒烟：起服务 → 向导 → 建会话 → 跑任务 → 完成。
#
# 覆盖的是「一条真实的用户路径」而不是单点接口：
#   setup/state 需要向导 → setup/complete 拿 token → 建会话 → 起 run →
#   轮询到终态 → 事件流有内容 → usage 端点可用。
# agent 用 shell（prompt 当 shell 命令真执行）、policy=allow-all（不产生人工审批），
# 因此全程不需要任何模型凭据。
#
# 用法：bash scripts/e2e.sh            （或 make e2e）
# 退出码：0 成功；非 0 失败（失败时会打印控制面日志与关键响应）。
#
# 兼容性：macOS 自带 bash 3.2，不用 bash 4 特性（无关联数组、无 ${var,,}）。

set -u

# ── 可调参数 ──────────────────────────────────────────────────
PORT_BASE=7905          # 从这里开始找空闲端口
RUN_TIMEOUT=60          # run 轮询超时（秒）
START_TIMEOUT=30        # 服务启动等待（秒）
JWT_SECRET="e2e-smoke-secret"

# ── 全局状态（cleanup 与错误报告要用）─────────────────────────
DATA_DIR=""
SRV_PID=""
LOG_FILE=""
BASE_URL=""

# ── 基础工具 ──────────────────────────────────────────────────

# 从 JSON 文本里提取顶层字符串字段。只依赖 grep/cut，不依赖 jq/python。
# 仅用于值里不含双引号/逗号的字段（id / token / 状态词），够本脚本用。
field_str() {
    printf '%s' "$2" | grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 | cut -d'"' -f4
}

# 从 JSON 文本里提取顶层标量字段（true/false/数字）。
field_val() {
    printf '%s' "$2" | grep -o "\"$1\"[[:space:]]*:[[:space:]]*[a-z0-9.\-]*" | head -1 \
        | sed "s/\"$1\"[[:space:]]*:[[:space:]]*//"
}

# HTTP 请求：$1=方法 $2=路径 $3=请求体（可空）$4=token（可空）
# 结果写进全局 REPLY_CODE / REPLY_BODY。
# 注意：不能用 body=$(http ...) 的写法——命令替换是子 shell，全局变量传不出来。
REPLY_CODE=""
REPLY_BODY=""
BODY_TMP="/tmp/e2e-body.$$"
http() {
    local method="$1" path="$2" body="$3" token="$4"
    local args=(-sS -o "$BODY_TMP" -w '%{http_code}' -X "$method" "$BASE_URL$path")
    if [ -n "$body" ]; then
        args+=(-H 'Content-Type: application/json' -d "$body")
    fi
    if [ -n "$token" ]; then
        args+=(-H "Authorization: Bearer $token")
    fi
    REPLY_CODE=$(curl "${args[@]}") || REPLY_CODE="000"
    REPLY_BODY=$(cat "$BODY_TMP")
    rm -f "$BODY_TMP"
}

die() {
    echo ""
    echo "✗ e2e 失败：$*" >&2
    if [ -n "$LOG_FILE" ] && [ -f "$LOG_FILE" ]; then
        echo "── 控制面日志（最后 100 行）──" >&2
        tail -n 100 "$LOG_FILE" >&2
    fi
    exit 1
}

cleanup() {
    if [ -n "$SRV_PID" ] && kill -0 "$SRV_PID" 2>/dev/null; then
        kill "$SRV_PID" 2>/dev/null
        wait "$SRV_PID" 2>/dev/null
    fi
    if [ -n "$DATA_DIR" ] && [ -d "$DATA_DIR" ]; then
        rm -rf "$DATA_DIR"
    fi
    rm -f "$BODY_TMP"
}
trap cleanup EXIT INT TERM

# ── 前置准备 ──────────────────────────────────────────────────

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BIN="$ROOT/bin/astra-server"
if [ ! -x "$BIN" ]; then
    echo "→ bin/astra-server 不存在，先编译…"
    go build -o bin/astra-server ./cmd/astra-server || die "编译 astra-server 失败"
fi

PORT=""
p="$PORT_BASE"
while [ "$p" -lt $((PORT_BASE + 30)) ]; do
    if ! lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
        PORT="$p"
        break
    fi
    p=$((p + 1))
done
[ -n "$PORT" ] || die "7905~7934 之间找不到空闲端口"

DATA_DIR=$(mktemp -d) || die "mktemp 失败"
LOG_FILE="$DATA_DIR/server.log"
BASE_URL="http://127.0.0.1:$PORT"

# 存储后端：默认 sqlite；设 E2E_DSN 走 postgres（如
# E2E_DSN="postgres://astra@127.0.0.1:5432/astra?sslmode=disable" bash scripts/e2e.sh）
# PG 模式要求库已建好（createdb astra），表由服务的幂等迁移自动创建。
STORE_KIND=sqlite
STORE_ARGS="-store sqlite"
if [ -n "${E2E_DSN:-}" ]; then
    STORE_KIND=postgres
    STORE_ARGS="-store postgres -dsn $E2E_DSN"
fi

echo "→ 启动 astra-server：port=$PORT data=$DATA_DIR agent=shell policy=allow-all store=$STORE_KIND"
"$BIN" -addr "127.0.0.1:$PORT" -data "$DATA_DIR" \
    -bootstrap=false -agent shell -policy allow-all $STORE_ARGS \
    -jwt-secret "$JWT_SECRET" >"$LOG_FILE" 2>&1 &
SRV_PID=$!

waited=0
while [ "$waited" -lt "$START_TIMEOUT" ]; do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/healthz" 2>/dev/null)
    if [ "$code" = "200" ]; then
        break
    fi
    if ! kill -0 "$SRV_PID" 2>/dev/null; then
        die "服务进程提前退出（可能端口被抢）"
    fi
    sleep 1
    waited=$((waited + 1))
done
[ "$code" = "200" ] || die "服务 ${START_TIMEOUT}s 内未就绪（healthz=$code）"
echo "  ✓ 服务就绪"

# ── 1. 向导：needsSetup=true ─────────────────────────────────

http GET /v1/setup/state "" ""
[ "$REPLY_CODE" = "200" ] || die "GET /v1/setup/state 返回 $REPLY_CODE: $REPLY_BODY"
needs=$(field_val needsSetup "$REPLY_BODY")
if [ "$needs" != "true" ]; then
    die "needsSetup=$needs，期望 true（全新数据目录应该还没初始化）"
fi
echo "  ✓ needsSetup=true"

# ── 2. 完成向导：201 + token ─────────────────────────────────

SETUP_BODY='{"slug":"e2e","name":"E2E 租户","email":"e2e@example.com","password":"e2e-password-8"}'
http POST /v1/setup/complete "$SETUP_BODY" ""
[ "$REPLY_CODE" = "201" ] || die "POST /v1/setup/complete 返回 $REPLY_CODE: $REPLY_BODY"
TOKEN=$(field_str token "$REPLY_BODY")
auto=$(field_val autoLogin "$REPLY_BODY")
if [ "$auto" != "true" ] || [ -z "$TOKEN" ]; then
    die "autoLogin=$auto token=${TOKEN:-<空>}，期望自动登录成功"
fi
echo "  ✓ 向导完成，拿到 token"

# ── 3. 建会话 ────────────────────────────────────────────────

http POST /v1/sessions '{}' "$TOKEN"
[ "$REPLY_CODE" = "201" ] || die "POST /v1/sessions 返回 $REPLY_CODE: $REPLY_BODY"
SESSION_ID=$(field_str id "$REPLY_BODY")
[ -n "$SESSION_ID" ] || die "会话响应里没有 id: $REPLY_BODY"
echo "  ✓ 会话 $SESSION_ID"

# ── 4. 起 run（注意字段是 input 不是 prompt）─────────────────

http POST "/v1/sessions/$SESSION_ID/runs" '{"input":"echo hello-e2e-smoke"}' "$TOKEN"
[ "$REPLY_CODE" = "201" ] || die "POST runs 返回 $REPLY_CODE: $REPLY_BODY"
RUN_ID=$(field_str id "$REPLY_BODY")
[ -n "$RUN_ID" ] || die "run 响应里没有 id: $REPLY_BODY"
echo "  ✓ run $RUN_ID 已排队"

# ── 5. 轮询 run 状态直到终态 ─────────────────────────────────

elapsed=0
status=""
while [ "$elapsed" -lt "$RUN_TIMEOUT" ]; do
    http GET "/v1/runs/$RUN_ID" "" "$TOKEN"
    [ "$REPLY_CODE" = "200" ] || die "GET /v1/runs/$RUN_ID 返回 $REPLY_CODE: $REPLY_BODY"
    status=$(field_str status "$REPLY_BODY")
    case "$status" in
        completed|failed|cancelled) break ;;
    esac
    sleep 1
    elapsed=$((elapsed + 1))
done
if [ "$status" != "completed" ]; then
    die "run 最终状态=${status:-<无>}（期望 completed），已等 ${elapsed}s"
fi
echo "  ✓ run completed（${elapsed}s）"

# ── 6. 事件里至少有一条消息增量或工具调用 ─────────────────────

http GET "/v1/runs/$RUN_ID/history" "" "$TOKEN"
[ "$REPLY_CODE" = "200" ] || die "GET history 返回 $REPLY_CODE: $REPLY_BODY"
if printf '%s' "$REPLY_BODY" | grep -qE '"message_delta"|"tool_call_start"|"tool_call_update"'; then
    echo "  ✓ 事件流有 agent 活动"
else
    die "history 里没有 message_delta / tool_call_* 事件: $REPLY_BODY"
fi

# ── 7. usage 端点能查到本次活动 ───────────────────────────────
# 说明：用量明细由 LLM 网关记账（source=gateway），shell agent 不走模型调用，
# 明细行可能为空；这里断言端点可用且返回完整报告结构（items 数组 + sum 汇总）。

http GET /v1/usage "" "$TOKEN"
[ "$REPLY_CODE" = "200" ] || die "GET /v1/usage 返回 $REPLY_CODE: $REPLY_BODY"
case "$REPLY_BODY" in
    *'"sum"'*|*'"items"'*) echo "  ✓ usage 报告可查（shell agent 不产生模型用量，明细可能为空）" ;;
    *) die "usage 响应缺少 items/sum: $REPLY_BODY" ;;
esac

# ── 摘要 ─────────────────────────────────────────────────────

echo ""
echo "════ e2e 通过 ════"
echo "  port      : $PORT"
echo "  session   : $SESSION_ID"
echo "  run       : $RUN_ID ($status)"
echo "  usage sum : $(printf '%s' "$REPLY_BODY" | grep -o '"sum":{[^}]*}')"
