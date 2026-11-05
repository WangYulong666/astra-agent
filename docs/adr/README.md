# ADR 索引

记录**为什么这么定**，以及**被否掉的是什么**。改动前请先读相关条目——很多方案看着更简单，是因为漏掉了已经踩过的坑。

| # | 决策 | 状态 |
|---|---|---|
| [0001](0001-use-acp-instead-of-pty.md) | 用 ACP 协议驱动 Agent，不用 PTY 编排 | 已采纳 |
| [0002](0002-agent-in-sandbox-with-acp-proxy.md) | Agent 跑在沙箱内，由 acp-proxy 桥接 | 已采纳 |
| [0003](0003-choose-deepseek-harness.md) | 选 DeepSeek Harness 作为一期 Agent 运行时 | 已采纳 |
| [0004](0004-trim-dsh-image.md) | 用 npm overrides 裁剪 dsh 镜像 | 已采纳 |
| [0005](0005-k3s-and-apisix.md) | k3s + APISIX 作为编排与入口 | 已采纳 |
| [0006](0006-no-backend-scaffold.md) | 不引入开源后台脚手架作为基座 | 已采纳 |
| [0007](0007-source-vendoring.md) | 第三方一律源码引入，可同步 | 已采纳 |
| [0008](0008-sandbox-provider-abstraction.md) | 沙箱做成可替换 provider（local / applecontainer / cube） | 已采纳 |
| [0009](0009-control-plane-shape.md) | M1 控制面：单进程 + 事件存储接口 + SSE | 已采纳 |
| [0010](0010-interactive-approvals.md) | 交互式审批：阻塞等待、双事件、超时即拒、重启归档 | 已采纳 |
| [0011](0011-web-workbench.md) | Web 工作台：手写样式、产物入库、客户端折叠时间线 | 已采纳 |
| [0012](0012-sandbox-ownership.md) | 强制边界只能有一层：`AgentSpec.SelfSandbox` 显式声明沙箱归属 | 已采纳 |

## 格式

每条 ADR 只回答四件事：背景是什么、决定了什么、后果是什么（好与坏都要写）、否掉了哪些替代方案以及为什么。

**坏处必须写。** 只列优点的 ADR 是宣传材料，不是决策记录。

## 什么时候加新条目

- 推翻了已有决策 → 新开一条，状态写「取代 00XX」，旧条目状态改成「已被 00XX 取代」（**不要删**）
- 只是补充细节、不改变方向 → 直接改设计文档，不新开 ADR

## 待定

以下几项**还没有决策**，不要在实现时默默选掉：

- 产物存储的路径规约与生命周期（M2 前定）
- 多租户隔离粒度：按 namespace / 按节点池 / 混合（M4 前定）
- 审批注册表从内存迁到 PG 的具体形态（多副本前定，见 ADR-0010）
- 是否保留 `byo_runtime`（用户自带 CLI 直接跑）模式
| [0013](0013-dsh-as-submodule.md) | deepseek-harness 改为 submodule + 稀疏检出 | 已采纳 |
