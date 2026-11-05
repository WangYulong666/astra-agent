# ADR-0010：交互式审批（挂起、超时、取消、重启归档）

- 状态：已采纳
- 日期：2026-10-29
- 关联：ADR-0009（控制面形状）、ACP `session/request_permission`

## 背景

M1 的权限策略是「预设自动决策」：`workspace-write` 按 tool kind 直接放行或拒绝，
每次决策落一条 `approval_req` 事件留痕。这样能证明「审批链路通」，但有一个根本缺陷：

**agent 从来没有真的停下来等人。** 事件里写下 `decision: allow` 的那一刻，结果早就
返回给 agent 了。这带来两个具体问题：

1. 演示不了「人不在场时任务会停住」——而这正是「沙箱化 agent」相对本地 CLI 的核心价值。
2. 一旦要接真实审批（M3），事件语义要改：现在的 `approval_req` 是「事后记录」，
   将来是「事前请求」。两者混在一张表里，历史数据没法区分。

## 决策

### 1. 审批阻塞在 ACP 请求上，不在控制面排队

`runner.decide` 在交互模式下阻塞在 `awaitHuman` 里，直接等 `session/request_permission`
的回复。**不**采用「先回 agent 一个占位、等审批完了再补一条消息」的做法。

理由：ACP 的语义就是「请求—应答」。占位返回等于告诉 agent「可以干了」，
它可能已经把命令跑完了——审批就退化成通知。要让人真的能拦住，只能让 agent 等。

代价写清楚：一条待审批会占住该会话的 `sem`（一会话同时只允许一个 prompt），
所以这期间同一会话不能再发新指令。这是对的——agent 本来就卡在那儿。

### 2. `approval_req`（请求）与 `approval_result`（结论）分成两条事件

事件流是 append-only 的，原地改写会抹掉「当时谁批的、隔了多久」。
分成两跳之后，前端（和事后审计）能还原完整时间线：

```
approval_req{decision:"pending", pending:true, expiresAt}
  → approval_result{decision:"allow"|"deny"|"timeout", by:"user"|"system"}
```

`approval_req` 在非交互策略下仍然保留 `decision` 字段（取值是 option kind），
所以两种模式的消费者是同一套代码。

### 3. 超时按拒绝处理，并且把 `timeout` 和 `deny` 分开记

默认 10 分钟（`ApprovalTimeout`）。超时**不能**当成「允许」——没人管的任务不该自己跑起来。

但要能和人工拒绝区分开：`timed out`（没人管）和 `rejected`（人明确不同意）
在事后复盘里是完全不同的两件事，事后追责与调策略都依赖这个区别。

### 4. 取消 = 先结审批，再发 `session/cancel`

顺序不能反。agent 正卡在 `request_permission` 上，很多实现要等这条请求有回复
才会去读取消通知。所以 `Cancel` 先 `releaseApprovals(deny)`，再发 ACP 取消。

### 5. `cancelGate`：解「取消比 prompt 还早」的竞态

真实踩到的现象：前端一看到 `run_state(running)` 就点了停止，但此时 `session/prompt`
请求可能还在路上。取消发给 agent 时它没有进行中的 prompt，于是取消被静默丢弃，
那一轮照跑到结束——用户看到「我按了停止，它还在跑，最后显示已完成」。

做法是把「用户要取消」和「prompt 已发出」放在**同一把锁**下（`cancelGate`），
保证二者必有一个先看到对方：

- 取消先落 → `beforePrompt` 直接不发 prompt，本轮记为 cancelled
- prompt 先发 → `requestCancel` 返回 true，走正常 ACP 取消

不用两个原子变量：两条独立写入没有顺序保证，交叉执行时两边都会以为自己该沉默。

另外，只要用户在 agent 结束前按过停止，即使 agent 刚好跑完也记成 `cancelled`。
「停止按钮的语义要可预测」比「精确区分是谁先到」更重要。

### 6. 控制面重启：归档而不是假装续跑

run 的执行体在内存里（goroutine + ACP 连接），进程一退就没了，但状态还是
`running` / `waiting_approval`。启动时 `Manager.RecoverInterrupted` 把它们标成
`failed`（`error: 控制面重启，本轮中断`）并补一条终态事件。

不续跑的理由：agent 会话里的中间状态、已经批过的权限都丢了，重放只会得到半截结果。
老实标失败让用户重发，比假装续上更可信。

配套的一处必须做：`ensureClient` 重连时**先发 cancel 再 resume**。
否则 agent 里那条卡在权限请求上的 prompt 永远等不到回复，下一次 prompt 会被
以「会话已有进行中的命令」拒掉——表现就是「重启之后这个会话废了」。

## 后果

好：

- 人真的能拦住 agent。拒绝后 agent 收到 `reject_once`，会走自己的「被拒」分支。
- 审批时间线可重建：请求时刻、决策、决策者、耗时都在事件流里。
- 重启不再是「会话废掉」，而是「上一轮失败、下一轮照常」。

坏 / 需要接受：

- **审批注册表在内存里。** 单副本阶段够用；多副本要用 PG 表 + 通知，否则
  A 副本收到的审批请求在 B 副本点不到。这是 M4 的活，接口已经收在 `Manager` 上。
- 一条待审批会占住整个会话（agent 本来就是串行的）。
- `awaitHuman` 阻塞的是 ACP 请求 goroutine，不阻塞事件流；但如果事件流在那期间
  没有新事件，前端只能靠 `waiting_approval` 状态事件知道「它在等我」，所以
  状态变更必须显式推事件——这是踩过的坑（只改 store，界面一直显示「执行中」）。

## 被否掉的方案

| 方案 | 为什么不用 |
|---|---|
| 控制面自己维护待审批队列，agent 侧立即返回 | agent 会继续执行，审批变成事后通知，拦不住任何东西 |
| 超时按「允许」 | 没人管的任务自己跑起来是安全事故，不是便利 |
| 把审批结果原地写回 `approval_req` 事件 | 事件流 append-only；改写会丢「谁在什么时候批的」 |
| 重启后自动重放未完成的 prompt | agent 侧上下文已丢，重放得到半截结果，比失败更误导 |
| 用 toolCallId 单键做审批注册表 | 不同 run 可能复用同一 id（重启后重放），会串台 |
