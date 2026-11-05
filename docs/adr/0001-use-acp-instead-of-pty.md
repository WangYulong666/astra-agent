# 0001. 用 ACP 协议驱动 Agent，不用 PTY 编排

- **状态**：已采纳
- **日期**：2026-10-28
- **取代**：v1 设计中的「PTY + PATH 垫片」方案（见 `docs/history/`）

## 背景

第一版方案（v1）把「Agent 会做什么」当成平台的责任来管：

- 用 **PTY** 起 agent 进程，解析它输出的 ANSI 转义序列，靠正则猜「现在是在等 prompt 还是在执行工具」
- 用 **PATH 垫片** 把 `rm` / `curl` 这类命令替换成自己的壳，拦下来让用户审批
- 用**正则匹配命令字符串**判断是否危险

这条路的问题不在于实现难，而在于**它在猜**。agent 输出格式一改、加个进度条、换个 spinner 字符，解析就崩。

## 决策

采用 **ACP（Agent Client Protocol）v1**，Agent 作为 ACP server，控制面作为 ACP client。

关键事实（已核实，非推测）：

| 项 | 事实 |
|---|---|
| 协议 | JSON-RPC 2.0 |
| 分帧 | **NDJSON，每行一条 JSON**——不是 LSP 那种 `Content-Length` 头 |
| 传输 | 规范只定义 stdio；Streamable HTTP 仍是 draft，但**明确允许自定义传输** |
| 版本 | 锁 `protocolVersion: 1`，v2 仍 experimental |
| 审批 | **`session/request_permission`** 由 agent 主动发起，client 阻塞回复后 agent 继续 |
| 工具语义 | `tool_call.kind` 直接标 `execute` / `edit` / `delete` / `read`，不需要解析命令字符串 |
| 生态 | 30+ agent 支持；Claude Code / Codex 走官方适配器，Gemini CLI / Cursor / OpenCode 等原生 |

## 后果

### 好的

- **审批变成协议级阻塞**，不再依赖字符串匹配。`request_permission` 不回复，agent 就停在那里，这是协议保证而不是我们的实现细节。
- **事件天然结构化**：`session/update` 带 `tool_call`、`plan`、`message_chunk` 等类型，前端可以直接渲染，不用解析文本。
- **换 agent 不伤架构**：层在协议上统一，dsh 换 Gemini CLI 只需要改启动命令和能力声明。

### 坏的

- **ACP 仍年轻**：v1 稳定但生态演进快，v2 已在 experimental。必须锁 v1，并把类型定义抄进自己仓库，不能直接依赖上游 npm 包的类型。
- **没有官方 Go SDK**：官方只有 TS / Rust / Python / Java / Kotlin。Go 侧要自己写 NDJSON 编解码 + JSON-RPC 分发，约 600 行。这可控，但确实是额外工作量。
- **能力面受 agent 限制**：dsh 的 `promptCapabilities` 里 `image` / `audio` / `embeddedContext` 全是 false，端侧传图片只能落盘让 agent 调工具读。

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| PTY + 正则解析（v1 原方案） | 在猜。格式变动即失效，且无法区分「工具调用」和「普通输出」 |
| 直接调各家 CLI 的 `--json` 非交互模式 | 没有审批通道。一旦要人工介入，就得杀进程重启，上下文丢失 |
| 自己写 Agent 循环 | 同时造 agent 和造平台，两个都做不好。平台的价值在编排、审批、多端、隔离 |
| MCP 当主通道 | MCP 是**工具**协议，不是**会话**协议。没有会话生命周期、没有权限请求、没有流式消息 |
