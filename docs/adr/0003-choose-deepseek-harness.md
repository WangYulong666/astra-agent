# 0003. 选 DeepSeek Harness 作为一期 Agent 运行时

- **状态**：已采纳
- **日期**：2026-10-28
- **相关**：ADR-0001（ACP）、ADR-0004（裁剪）

## 背景

架构上 Agent 是可替换的（层在 ACP 上统一），但一期必须选一个把链路跑通。候选四个，都实测过体积或 ACP 能力：

| 候选 | ACP 接入方式 | linux/x64 落盘 | 需 Node | 许可与源码 |
|---|---|---|---|---|
| **DeepSeek Harness** | 第一方插件 `dsh --profile acp` | 826MB（裁剪后 138MB） | 是 | **MIT，可自编译** |
| CodeBuddy Code | 内建 `--acp`（**隐藏参数**） | 68MB → 裁后 ~28MB | 是 | 闭源，打包混淆 JS |
| Claude Code | 适配器 `@agentclientprotocol/claude-agent-acp` | 293MB（单 native 二进制） | **否** | 闭源二进制 |
| Gemini CLI | 原生 `gemini --acp` | — | 是 | Apache 2.0，但发布节奏在 Google |

## 决策

**一期用 DeepSeek Harness（dsh），`dsh --profile acp` 启动。**

| 项 | 事实 |
|---|---|
| 项目 | `deepseek-ai/deepseek-harness`，MIT，2026-08 开源 |
| 版本 | 当前 `v0.2.1-rc.2`（**仍是预发布**） |
| ACP | **第一方**插件 `@deepseek-ai/dsh-acp`，不是第三方适配器 |
| 传输 | NDJSON over stdio，日志严格走 stderr |
| 会话 | 支持跨进程重启的 `list` / `resume` / `close`；**刻意不做** delete / fork / transcript 回放 |
| MCP | `session/new` 时挂载，stdio + streamable HTTP |
| 权限 | 三档预设 `read-only` / `workspace-write` / `danger-full-access`，每档是「沙箱 + 审批」成对定义 |
| 运行时 | Node ≥ 22.15（源码构建需 ≥ 24.2） |

### 选它的四个理由

1. **MIT 且可自行编译** —— 符合「全组件源码可控」红线，不依赖任何一家厂商的发布节奏
2. **中文模型优先** —— 默认 route 到 DeepSeek，token 成本比 Claude / Gemini 低一个量级
3. **插件化内核** —— 未来平台专属能力（产出物上报、租户配额）可以写成 DSH 插件，比只能走 MCP 更深入
4. **第一方 ACP** —— 不需要「适配器 + npx 拉包」，离线镜像简单

### 「不做 transcript 回放」是优势，不是缺陷

dsh-acp 刻意只提供标准 ACP v1 界面，不提供 DSH 专属呈现（plan 卡片、todo、标题）和 transcript 回放。

我们本来就把 `session/update` 全量落到 PG 事件表。agent 不重放历史，意味着**它不持有「第二份真相」**——重启后前端看到的视图和库里的状态必然是同一份。这比让 agent 重放更可靠：重放依赖 agent 日志是否完整，而事件表是平台自己的资产。

所以重连路径是 `session/resume`（**只恢复执行状态**），历史从 `run_events` 重建。

### 权限预设怎么映射到平台审批

| DSH 预设 | 平台策略 |
|---|---|
| `read-only` | 自动批准只读工具，写操作直接拒 |
| `workspace-write` | **默认**。bash / 写文件限 cwd，越界时 dsh 自己发 `request_permission` |
| `danger-full-access` | **永不使用**。VM 已是隔离边界，这一档等于放弃内层防线 |

关键：**dsh 的 `workspace-write` 已做了内层约束（路径越界），我们的审批只兜外层（出网、跨租户、危险命令）**。这是纵深防御，不是重复。

## 后果

### 好的

- 许可红线守住了，且成本可控
- 权限模型与平台审批天然对齐，不需要自己造一套命令分类
- 换 agent 的迁移成本低（ACP 是标准协议），二期想换 CodeBuddy 只需改启动配置和能力声明

### 坏的

- **仍是预发布**（`v0.2.1-rc.x`，仓库自带"兼容性破坏"警告）→ 镜像里**钉死精确版本**，升级当独立任务做
- **需要 Node ≥ 22.15** → 镜像比单二进制方案大（裁剪后仍 ~138MB）
- **ACP 能力面保守**：`image: false`、`audio: false`、`embeddedContext: false`；MCP 只支持 `http`（无 sse）；有 `resume`/`list` 但无 `loadSession`
  → **端侧要传图片或附件，只能落盘让 agent 调工具读，不能走 ACP 原生 image block**。这会直接影响媒体输入的产品设计
- 一期拿不到 plan / todo 的结构化展示，UI 要自己从 `session/update` 拼

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| **CodeBuddy Code** | 体积最轻（~28MB）、ACP 能力最全（`loadSession: true`、MCP 支持 sse + http），但**闭源、打包混淆 JS、无源码仓库**，违反红线。另外 `--acp` 是**隐藏参数**（help 里没有），非公开契约，升级可能消失 |
| **Claude Code** | 唯一不需要 Node（293MB 单静态二进制，可上 distroless），但闭源 + 需适配器进程 |
| **Gemini CLI** | Apache 2.0、原生 ACP、单个二进制，工程上很省事。否掉是因为模型生态与成本（中文场景 DeepSeek 便宜一个量级）、插件化深度不足 |
| `@jeremy9682/dsh-acp` | **权限 auto-answer（自动答）**，等于没有人工审批；无 resume、无 MCP 挂载。用错这个，整套审批链是空的 |
| `@openma/deepseek-harness-acp` | 功能最全（load + resume、图片、steering），但第三方 beta。**留作二期备选** |
| `hanxu131-dshacp` | 刻意不实现 `fs/`、`terminal/`、`session/set_mode`，能力不全 |

**一期只用官方 `@deepseek-ai/dsh-acp`。** 搜的时候会跳出三个第三方同名包，装错会毁掉整个审批设计。
