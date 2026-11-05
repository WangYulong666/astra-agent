# ADR-0012：强制边界只能有一层——沙箱归属显式化

- 状态：已采纳
- 日期：2026-10-29
- 关联：ADR-0008（沙箱 provider 抽象）、ADR-0010（交互式审批）、`AgentSpec.SelfSandbox`

## 背景

接真实模型（dsh + DeepSeek）之后遇到一个**跑不通**的问题：`local` provider 用
`sandbox-exec` 把 agent 关在会话目录里，而 dsh 自己也用 Seatbelt 把 bash 工具
关在「工作区 + /tmp」里。两层叠在一起，结果是 dsh 的每条命令都失败：

```
Error: sandbox backend unusable
sandbox-exec: sandbox_apply: Operation not permitted
```

原因不是配置错，是 macOS 的内核限制：**`sandbox_apply` 在已被沙箱化的进程里直接拒绝**。
沙箱不可嵌套。这不是「要调参数」的问题，是这条路根本不存在。

更糟的是失败后的连锁反应：模型看到「沙箱不可用」，于是按 dsh 的提示用
`sandbox_permissions: danger-full-access` 申请扩权——每一次都触发一次审批，
每一条命令都要人点一次「允许」。

## 决策

**把「谁负责强制隔离」变成 agent 声明的属性，而不是平台默认行为。**

```go
type AgentSpec struct {
    // SelfSandbox 表示 agent 自带文件级沙箱，外层不要再套。
    SelfSandbox bool
}
```

`local` provider 据此决定要不要包 `sandbox-exec`：

```go
Strict: l.strictAvailable() && !spec.SelfSandbox
```

| agent | SelfSandbox | 强制隔离由谁提供 |
|---|---|---|
| `acp-shell-agent`（开发用） | false | 平台：`sandbox-exec` 写限制 |
| `dsh --profile acp` | true | agent：Seatbelt `workspace-write`（工作区 + /tmp） |

同时把 dsh 的 approval 钉回 `ask`（它的默认值会从沙箱模式推导，宽模式直接变 `never`）：
**隔离可以外借，审批不能外借。**

## 后果

好的：

- 真实模型能在本地跑通了。实测同一个会话里写完 `fizzbuzz.py`、`python3` 跑出 30 行输出。
- 审批回到「模型申请扩权时才问」，而不是每条命令都问——这才是合理的粒度。
  实测：让它写工作区外的文件 → 第一次被 Seatbelt 拒绝 → 模型带 justification 重试
  → 控制面挂起 → 人在浏览器点「允许」→ 文件写成功。
- 生产形态不受影响：cube 的 microVM 就是边界，容器内 agent 本来就该跑在无约束态。
  那时 `SelfSandbox` 为 false，边界由 VM 提供。

坏的 / 需要记住的：

- **本地这一层边界从「平台代码」变成了「agent 配置」**。dsh 的 profile 改坏，
  本地就没有写限制了。缓解：`chain` 里那份 `astra-trim.yml` 跟着二进制走，
  并且只做加法（不禁用 sandbox-policy 本身）。
- 两层不能同时开这件事**只能靠约定**——没有编译期或运行期检查能发现「你配了两层」。
  现在靠的是 `SelfSandbox` 的注释与这份 ADR。
- dsh 的 ACP 权限桥只发 `toolCallId`（不带 title / kind），审批卡片要靠同
  `toolCallId` 的 `tool_call_start` 回查意图。前端已按这个做（`ApprovalCard` 的 `tool` 入参）。

## 否掉的替代方案

**1. 保留平台外层，把 dsh 的沙箱关成 `danger-full-access`。**

这是最先试的方案，跑得通，但审批彻底失效：dsh 只在「申请比当前更宽的权限」时才咨询
审批，模式已经是全权时没有更宽的可申请。结果是「一个能跑但没有审批的 agent」。
对 demo 勉强够，对产品是把核心特性砍掉了。

**2. 保留 dsh 内层，平台降级为「无隔离」并告警。**

与最终方案的实际效果相同，但把「无隔离」当成降级路径。区别在语义：现在是
「agent 声明自己负责」，不是「平台放弃负责」。前者可以写进 agent 契约，后者只是
一个迟早会被忽略的 warning。

**3. 给 local provider 换一个允许嵌套的沙箱（如 bwrap / Landlock）。**

macOS 上没有。Linux 上 bwrap 嵌套也是麻烦事（需要额外的 namespace 权限），
而本地开发主力是 macOS。
