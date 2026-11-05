# ADR-0009：M1 控制面的形状（单进程 + 事件存储接口 + SSE）

- 状态：已采纳
- 日期：2026-10-29
- 关联：ADR-0002（agent 在沙箱内 + acp-proxy）、ADR-0008（沙箱 provider 抽象）

## 背景

M1 的目标是「浏览器能看到 agent 的流式输出」。要做到这点，需要三件东西同时成立：

1. 一个能把 prompt 送到沙箱里的 agent、并把增量拿回来的执行体（runner）
2. 一个「事件不丢、可按序号续传」的存储（断网重连不能看到半截流）
3. 一个能实时推送的通道（SSE）

而这三件事在没有模型凭据的机器上都验证不了——`dsh` 没有 DeepSeek API key 时，
`session/prompt` 直接返回 `-32603: no API key for provider route`。这会让
「改一行控制面代码 → 验证」变成「先解决凭据」。这个成本必须消掉。

## 决策

### 1. 单进程，不拆 api / worker

`cmd/astra-server` 一个进程同时跑 HTTP 与 runner。

拆成两个进程的收益（独立扩缩容）只有在多副本时才成立，而多副本要求 PG + 分布式锁，
那是 M4 的事。现在拆只会多出跨进程调试成本。**接口边界已经按可拆的方式划好**：
`api` 依赖 `runner.Manager` 与 `store.Store` 两个接口，不依赖它们的具体实现。

### 2. 事件存储定成接口，M1 用 JSONL

`internal/store` 定义 `Store`，语义照架构文档的 PG 表（`next_seq` 原子分配、
`PRIMARY KEY(run_id, seq)` 幂等）。实现是每个 run 一个 JSONL 文件，追加写 + fsync。

不用 SQLite 的理由：M1 只需要「按 seq 追加、按 after_seq 回放」两件事，
换不来 SQL 的收益，却要背上 cgo 或一个几十 MB 的纯 Go 驱动。
PG 实现必须在多副本阶段补上，届时上层不改——这是现在定成接口的唯一原因。

### 3. SSE 的三条硬语义

这三条都是踩过才知道必须写死的：

- **先订阅，再回放历史。** 反过来的话，读历史期间的实时事件会永久丢失。
  推送时丢弃 `seq <= 已推过的`，处理订阅与回放的重复。
- **终态即返回。** run 结束后服务端主动关连接。若不关，浏览器的 `EventSource`
  会永久挂在一条不会有新数据的连接上；客户端还会因为「连接被关」而每秒重连一次，
  对已结束的 run 形成无限重连。
- **缓冲满时主动断开并告知重连**（`event: overflow`），而不是无限堆积内存。
  客户端凭 `Last-Event-ID` / `after_seq` 续传——丢的是通知，不是数据。

心跳走注释帧（`: ping`），因为中间任何一层代理在 60 秒静默后都会掐连接，
而一次长命令可能几分钟没有输出。

### 4. 权限用预设，但每次决策都留痕

M1 用 `workspace-write` 预设自动决策（允许 execute/read/edit，拒绝 fetch/delete），
不上人工审批。但**每次请求都落一条 `approval_req` 事件**，含 policy 名与决策结果。

这样 M3 把「自动决策」换成「等人点」时，历史数据不用迁移，前端也只是多一个按钮。

### 5. 本地开发用 shell agent，不是 mock

`cmd/acp-shell-agent` 把 prompt 当 shell 命令，在沙箱里**真执行**，stdout/stderr
逐行回传成 `agent_message_chunk`。

它与 `acp-mock-agent` 的区别是用途：mock 用于**单元测试**（输出完全确定、可控分块速度），
shell 用于**开发与演示**（真命令、真退出码、真取消）。两者都不需要模型凭据。

## 实测（Mac mini M4 / macOS 26.2 / arm64，provider=local）

```
curl POST /v1/sessions                 → 会话 + 沙箱起来了
curl POST /v1/sessions/{id}/runs       → run
curl -N  /v1/runs/{id}/events          → SSE
```

| 验证 | 结果 |
|---|---|
| 事件序列 | `run_state(running)` → `tool_call_start` → `approval_req` → `message_delta` → `tool_call_update(exitCode)` → `run_state(completed)` |
| 真实流式 | 每秒一条（时间戳 29→30→31→32），不是结束后一次性吐出 |
| 取消 | `POST /cancel` → 202；进程组被 kill，`{"cancelled":true,"exitCode":-1}`，终态 `cancelled` |
| 重启控制面 | 会话列表与旧 run 的事件全部回放；**在同一会话继续发命令成功**（沙箱按 meta 重建 + agent 会话 resume） |
| 事后连 SSE | 纯回放路径，拿完终态后连接自动关闭 |
| `after_seq` 续传 | 只返回该序号之后的事件，条数对得上 |

单元测试 `internal/api`（4 项）：回放并关闭、`after_seq` 续传、会话生命周期与 404、空输入校验。

## 代价与已知欠缺

- **没有鉴权。** 绑 `127.0.0.1`，仅本机。任何对公网暴露都需要先补认证（M3）。
- **单副本。** 重启会丢掉「沙箱句柄」，靠 provider 的 `Acquire` 重建；正在跑的 run 会中断。
- **没有分层事件压缩。** 高频增量按 50ms 窗口合并，但长跑任务的 JSONL 会一直长下去，
  需要定期归档（M2 的离线缓存会顺手处理）。
- **`docker` 路径未验证。** `images/agent/Dockerfile` 与 `cube` provider 都还没有在
  linux/x64 上跑过，首次启用前先 `doctor`。
