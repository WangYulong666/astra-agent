# 自闭环 personal agent：架构与本轮迭代契约

> 裁判负责人定稿，2026-11-08。所有并行任务以本文的接口与文件边界为准。

## 0. 为什么要做这个

竞品调研（WorkBuddy / Meta Muse / Grok Bot / Manus / Claude Code / Devin / DGM-SICA）的结论：

- **能力发现**已收敛到渐进式披露三层：`name+description` 常驻 → 命中才读 SKILL.md → 资源按需加载。
- **能力生产**最高商品档是「成功轨迹 → 自动生成 SKILL.md」（Manus）与「演示一遍 → routine」（Grok Bot）。WorkBuddy 官方仍是「人写 + 市场 + 描述生成」，**从轨迹自动生成 skill 查不到**——这恰好是我们已有的 `internal/distill`，是差异化点，不是要补的短板。
- **生命周期**被三家拆着做：skill-creator 管验证、开放平台管发布审核、Devin Dreaming 管淘汰，**没人打通**。
- **自闭环**商品化最高到「自动写 skill/routine + 自动整理记忆 + 人工发布」；「自动改平台代码并回归」只见于研究，且已出现伪造执行日志、移除检测标记等评测作弊 → **明确不做**。
- **信任边界**最值得抄的是执行域/安全域分离 + 出网哨兵 + 凭据占位符；Grok Bot 被点名的「隔离按用户不按 Bot」是多租户设计必须避开的坑（我们每会话独立沙箱，天然没有）。

现状审计的结论更直接：**平台把零件都做出来了，但主链一环未接**。

```
跑任务 → 轨迹 → [断] 自动沉淀 → [断] 注册/发现 → [断] 沙箱内真能调用 → [断] 结果回灌 → 下一轮
```

本轮目标：把这条链**接通并且可验证**，不追求自动改代码。

## 1. 闭环定义

```
                        ┌──────────────── 渐进式披露 ────────────────┐
                        ↓                                           │
  ① 原子能力（CLI / API）→ ② 能力注册表 /v1/capabilities             │
                                    ↓                                │
  ③ run 执行（沙箱内 astra-cli 真能跑，会话级受限凭据）──────────────┤
                                    ↓                                │
  ④ 终态 → 自动沉淀 skill 草稿 → 人工 accept → 写索引 + 广播          │
                                    ↓                                │
  ⑤ 失败 run → 结构化诊断 → 回灌下一轮 prompt / 记忆                   │
                                    ↓                                │
  ⑥ doctor 自检闭环前提（CLI 可执行 / skills 可读 / 控制面可达）        │
                                    └────────── 回到 ① ──────────────┘
```

「人工 accept」是刻意保留的闸门：竞品里自动发布 skill 的都配了审核流，我们单人自部署场景下用 accept 一步替代，不做全自动发布（防坏 skill 污染）。

## 2. 三个架构决策

### 2.1 单一事实来源：能力注册表，不手抄

`internal/capability` 汇总四类事实，输出**机器可读**清单：

| 来源 | 内容 | 怎么拿 |
|---|---|---|
| `openapi.yaml` | 全部 API 路由 + operationId + 权限 | `api.SpecRoutes()`（主控在 api 包导出，复用现有 `parseYAMLSubset`） |
| CLI 命令表 | `astra-cli` / `admin-cli` 全部子命令 | `internal/client/clikit` 新增 `CommandSpec` 结构化清单（50 条）。`TestCLIRegistryCoversCommands` 用源码正则抓所有 `case "..."`，新增命令忘了登记就红；help 文本暂保留原样，由这道闸门保证不漂移 |
| skills 目录 | 静态 skill + distill 产出的动态 skill | `internal/skillhub` 扫描 `SKILL_DIR` 与仓库 `skills/` |
| `/v1/meta` | 能力开关 on/off | app 装配时注入 |

`GET /v1/capabilities` 默认只返回 `name + kind + description + enabled`（**第一层披露**）；
`?detail=full` 才返回 endpoint / requiredEnv / authAction / requires / source（第二层）；
`GET /v1/capabilities/{name}` 返回单项全文（第三层，含 SKILL.md 正文）。

新增命令或路由忘了登记 → 契约测试红（复用 `TestCodeRoutesListMatchesSource` 的思路，加一条 `TestCLIRegistryMatchesMainHelp`）。

### 2.2 会话级受限凭据：沙箱 key，不是平台 token

绝不给沙箱注入长期 token 或平台 JWT。新增 `auth.Service.IssueSandboxKey(tenant,user,session,ttl)`：

- scope 固定 `sandbox`，与平台 JWT（scope 空）、LLM 虚拟 key（scope `llm`）三者互斥，共用同一签名密钥但校验路径分开；`Authenticate` 继续拒绝 scope 非空的 token。
- API 侧新增窄白名单分支，**实际放开的集合**以 `internal/api/sandbox_authz.go` 的 `sandboxPathAllowed` 为准（且仅 GET）：
  `/v1/meta`、`/v1/capabilities`、`/v1/capabilities/{name}`、
  `/v1/skills`、`/v1/skills/{name}`（`drafts*` 管理子命名空间除外）、
  `/v1/memory/*`（handler 按登录身份的 tenant+user 过滤，沙箱 key 填同源 `Principal`，无需额外路径校验）。
  **白名单外一律拒绝**（`authed` 直接 403，不是 404）。
- **会话存在性校验 = 吊销机制**：`trySandboxAuth` 验签通过后用 `Store.GetSession(ctx, claims.SessionID)` 复核，查不到、或 `sess.Status == store.SessionDeleted` → 判沙箱凭据失效。`DELETE /v1/sessions/{id}` 先软删标 `SessionDeleted`、再删目录，两种状态都拦得住，所以**删会话即吊销**，无需全局吊销表/黑名单。TTL 默认 2h 只作兜底，限制 key 泄露后的最大可利用窗口。`Store` 为 nil 时 fail-closed，不留静默放行。
- **泄露处置 SOP**：删掉对应会话即可，不用轮换 JWT 密钥（不会全员掉线）；轮换密钥仅用于密钥本身泄露。
- `/v1/sessions/{id}` 与 `/v1/runs/{id}/*` **下一轮才考虑**放开：那需要逐条路径做 session/run 归属校验，漏一条就是横向越权；且 `sandboxSessionID` 注入的 context 值目前没有生产消费者（只有测试读它）。本轮不扩大白名单，也不承诺跨会话 404。

`CreateSession` 注入 `ASTRA_SERVER`（回环可达地址）、`ASTRA_TOKEN`（sandbox key）、`ASTRA_CLI_BIN`、`ASTRA_SANDBOX_ID`；
控制面回环端口自动进 `NetworkPolicy.ExtraAllow`；`astra-cli` 二进制由 app 在启动时编/拷到 `<dataDir>/bin/`，通过 `AgentSpec.Env` 给绝对路径，并在 `childEnv` 里加进 PATH。

### 2.3 Skill 生命周期：目录统一 + 索引 + 校验

- **目录统一**：`SKILL_DIR`（默认 `~/.astra/skills`）既放 distill 产物，也放静态 skill。仓库 `skills/` 作为「随发行版内置」的来源，启动时同步进 `SKILL_DIR`（不覆盖人手改过的）。
- **索引**：`SKILL_DIR/index.json`（原子写、0600）记录 name / description / version / requires / source / status / updatedAt。accept / reject / 新建都刷新它，并发安全。
- **requires 校验**：`astra-cli` 是否可执行、所需能力开关是否 on。不满足的 skill 在清单里标 `unavailable` 并给出原因，**不下发给 agent**，也不静默假装可用。
- **广播**：accept 后 `Hub.Publish` 一条事件（新类型 `skill_published`），前端与 CLI 可订阅刷新。
- **自动沉淀**：`distill.Attach(hub, ...)` 订阅 run 终态，成功工具调用 ≥2 且该 run 未沉淀过时自动生成草稿；开关 `DISTILL_AUTO`（默认 true），去重键 = runID。人保留 accept 权。

## 3. 失败诊断与回灌（不自动改代码）

`internal/insight` 订阅 run 终态：

- `status=failed/cancelled` → 生成结构化诊断：`{runID, error, lastTool, toolCount, activitySummary, suggestion}`，落 `insights.json`（原子写、上限 500 条）。
- `suggestion` 是**规则式**（错误分类：超时 / 权限拒绝 / 出网被拦 / 命令不存在 / 模型错误 → 对应一句可执行建议），不调 LLM。
- `GET /v1/insights`（运营页 + CLI `logs insights`）。
- **回灌**：`StartRun` 时若同会话最近有未消诊断，把最相关一条作为 prompt 前缀（明确标注「上轮失败上下文」，不篡改用户输入，可用 `INSIGHT_REPLAY` 关闭）。
- 明确不做：自动生成改进任务、自动改平台代码、自动重试改参数。研究里这类已出现评测作弊，且我们单人自部署没有审核层兜底。

## 4. 文件边界（并行任务）

| 任务 | 独占 | 禁止改 |
|---|---|---|
| T1 能力注册表 | `internal/capability/**`、`internal/client/clikit/registry.go` | `cmd/*/main.go`、`internal/api/**`、`internal/app/**` |
| T2 Skill 生命周期 | `internal/skillhub/**`、`internal/distill/**`、`cmd/astra-cli/cmd_skills.go` | `internal/api/**`、`internal/app/**` |
| T3 诊断回灌 | `internal/insight/**` | `internal/runner/**`、`internal/api/**`、`internal/app/**` |
| T4 沙箱凭据与可执行性 | `internal/auth/**`、`internal/sandbox/**`、新建 `internal/api/sandbox_authz.go` | `internal/api/api.go`、`internal/runner/**`、`internal/app/**` |
| T5 前端闭环页 | `web/src/**` | 后端所有包 |

**主控独占**：`docs/design/**`、`internal/api/api.go`、`internal/app/**`、`internal/runner/manager.go`、
`internal/api/openapi.yaml`、`internal/api/openapi_test.go`、`cmd/astra-cli/main.go`（仅命令表接线）、
`cmd/admin-cli/main.go`、`Makefile`、`go.mod`。

跨包依赖一律走**主控写的 adapter / 窄接口**，agent 不得互相 import 对方的新包（避免装配环）。

## 5. 最小接口契约（Go，主控写骨架，agent 填实现）

```go
// internal/capability/capability.go
type Item struct {
    Name, Kind, Description string   // kind: api | cli | skill
    Enabled   bool
    Endpoint  string                 // detail=full 才填
    RequiredEnv []string
    AuthAction  string
    Requires    []string
    Source      string               // openapi | astra-cli | admin-cli | builtin | distilled
    Unavailable string               // 非空说明为什么不可用
}
type Registry interface {
    List(ctx context.Context, detail bool) ([]Item, error)
    Get(ctx context.Context, name string) (Item, error)   // 含 SKILL.md 正文 Body
    Refresh(ctx context.Context) error
}
func Open(cfg Config) (Registry, error)

// internal/skillhub/skillhub.go
type Skill struct {
    Name, Description, Version, Body, Source, Status string
    Requires []string
    Unavailable string
    UpdatedAt time.Time
}
type Hub interface {
    List(ctx context.Context) ([]Skill, error)
    Get(ctx context.Context, name string) (Skill, error)
    Refresh(ctx context.Context) error       // 重建 index.json
    Publish(ctx context.Context, name, body string, requires []string) (string, error)
}
func Open(cfg Config, hub *eventbus.Hub) (Hub, error)

// internal/insight/insight.go
type Diagnosis struct {
    RunID, SessionID, Status, Error, LastTool, Suggestion string
    ToolCount int
    Blocked, Failed int
    CreatedAt time.Time
}
type Store interface {
    Recent(ctx context.Context, sessionID string, limit int) ([]Diagnosis, error)
    All(ctx context.Context, limit int) ([]Diagnosis, error)
    ForRun(ctx context.Context, runID string) (Diagnosis, error)
}
func Attach(hub *eventbus.Hub, st store.Store, act Activity, dataDir string, lg *log.Logger) func()
func Open(dataDir string) (Store, error)
```

`Activity` 是窄接口（`Summary(ctx, runID) (ActivitySummary, error)`），由主控适配 `activity.Ledger`。

## 6. 验收（本轮必须真跑通）

1. `GET /v1/capabilities` 能列出 API / CLI / skill 三类，数量与 `codeRoutes`、两个 CLI 命令表、skills 目录一致（契约测试防漂移）。
2. `GET /v1/skills` 能看到 **distill accept 后新增**的 skill（不是硬编码 3 个）。
3. **真机**：起服务 → 建会话 → 在沙箱里执行 `astra-cli whoami` 与 `astra-cli skills list`，返回 200 且身份是该会话；**白名单外一律拒绝**（含 `/v1/sessions/*`、`/v1/runs/*`，API 返回 403）——本轮未放开这些路径，也没有逐路径归属校验，故不再写「跨会话 404」。删掉会话后，已签发的 sandbox key 立即失效（会话存在性校验），无需轮换密钥。
4. 一条成功的 run（≥2 工具调用）结束后，`GET /v1/skills/drafts` 自动多一条草稿（不用手工 POST）。
5. 一条失败 run 后 `GET /v1/insights` 有结构化诊断；下一次 `StartRun` 的 prompt 带上一轮失败上下文（有开关可关）。
6. `make doctor` 追加三项闭环自检（CLI 可执行 / skills 可读 / `/v1/capabilities` 200）。
7. gofmt / vet / 全量 test / race / 前端 / linux-amd64 交叉编译 / CI 全绿。

## 7. 明确不做

- 自动改平台代码并回归（研究与评测作弊风险）
- 自动发布 skill（保留人工 accept 闸门）
- HTTPS MITM / 真 Langfuse 端到端（上一轮已标注，另排）
- 多副本共享 `index.json` / `insights.json`（当前是单机文件，PG 化另排）
