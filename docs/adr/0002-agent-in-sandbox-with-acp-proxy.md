# 0002. Agent 跑在沙箱内，由 acp-proxy 桥接

- **状态**：已采纳
- **日期**：2026-10-28
- **相关**：ADR-0001（ACP）、架构设计方案 §2.2

## 背景

确定用 ACP 之后，第一个工程问题是：**agent 进程跑在哪，控制面怎么连它。**

最直接的做法是 worker 直接 `exec` agent，接管它的 stdin/stdout pipe。这个做法有两个硬故障：

1. **worker 重启，pipe 就断** —— agent 变成孤儿进程，会话不可恢复
2. **worker 扩缩容** —— 滚动更新会把正在跑的长任务直接杀掉

## 决策

**Agent 跑在沙箱内**，沙箱内额外跑一个 `acp-proxy`（约 200 行）：

- 拉起 agent 子进程，接管它的 stdio
- 把 stdio 桥接成 **WSS** 端点
- 保证**单客户端互斥**：同一时刻只服务一条连接，新连接抢占时旧连接收到 409
- **不缓存帧**。断线重连后的历史由 agent 重放（若支持）或由平台事件表重建，proxy 不持有状态

worker 通过 `wss://8080-<sandboxID>.<domain>/acp` 直连。

### 绑定关系（不是可选项）

| 关系 | 基数 | 约束来源 |
|---|---|---|
| Session → Sandbox | 1 : 1（活跃期） | 上下文 / 文件系统 / shell 状态都是会话私有的 |
| Sandbox → Agent 进程 | 1 : 1 | 不做多实例复用，共用沙箱等于把两个工作区叠在一起 |
| Sandbox → acp-proxy | 1 : 1 | proxy 只桥接自己那个 agent |
| ACP 连接 → worker | 1 : 1（同一时刻） | JSON-RPC id 命名空间冲突、权限请求被任一方抢答 |
| **Worker → 会话** | **1 : N** | 一个 worker 并发持有多条独立 WSS |

**注意**：这是「一条连接同一时刻只能被一个 worker 持有」，不是「全集群只有一个 client」。集群有 N 个 worker，各持有不同会话的连接，用 PG advisory lock 按 `session_id` 保证互斥。

### worker failover

```
worker 崩溃 → 它持有的所有会话转 reconnecting
          → lease（15s）过期，其他 worker 抢到 advisory lock
          → 重连该会话的 proxy（agent 未重启，会话在内存中）
          → 直接继续，不需要 resume
          → 仅当沙箱/agent 也重启过，才调 session/resume 从持久化恢复
```

一条会话的连接始终只有一条，worker 之间是**接管**而不是**共享**。

## 后果

### 好的

- **worker 变成无状态可重启**：连接断了重连即可，agent 和它的上下文还在沙箱里活着。
- **沙箱 pause 就是免费 checkpoint**：CubeSandbox 是 VM 内存快照，pause 冻结整个 agent 进程树——模型上下文、文件句柄、shell 状态全在。空闲 5 分钟 pause、新指令到达 resume，几十毫秒继续。v1 里要自己做的上下文序列化、工具输出持久化、恢复重放，全省了。
- **隔离边界下沉**：agent 可能执行 `rm -rf`、可能装恶意包，这些都在 microVM 里，出不来。

### 坏的

- **多一个进程要维护**：proxy 挂了 = 会话中断（但可重建，session 不变只换 sandbox）。
- **多一跳网络**：WSS 桥接比直接 pipe 多几毫秒延迟。对流式输出无感，但要接受。
- **端口要错开**：实测某些 agent（如 CodeBuddy `--acp`）会额外监听本地端口，沙箱内必须与 proxy 分配不同端口，否则 `EADDRINUSE`。
- **Worker 扩容不能按 CPU 算**：一条 ACP 连接绝大部分时间在等 LLM 返回，几乎不占 CPU。按 CPU 做 HPA 会导致会话数涨到几百时 CPU 还是平的。正确指标是**每 worker 会话数 / 200**（连接硬上限）。这也是 worker 与 api 必须分开扩缩容的原因——负载特征完全相反。

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| worker 直接 exec agent 并持有 pipe | worker 重启即断，agent 变孤儿；扩缩容杀长任务 |
| worker 走 envd process RPC 转发 stdio | 控制面要持有子进程 pipe，同样是 worker 重启即断。envd 的 process 是给「跑完就结束」的命令用的，不是给长生命周期交互会话用的 |
| 沙箱内 agent 直接对外暴露（让端侧直连） | 一个 agent 只有一路 stdin/stdout，两路 client 接入会让 JSON-RPC id 冲突、权限请求被任一方抢答。而且端侧要拿到沙箱地址，等于绕过控制面的鉴权和审计 |
| proxy 缓存帧做「断点续传」 | 缓存引入第二个真相源，和 agent 内部状态必然不一致。历史的权威来源是 PG 事件表 |
