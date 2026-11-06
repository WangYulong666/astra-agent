# 能力扩展 v3 契约：把上一轮「明确没做」的补齐

对标 Muse / Grok Bot 的差距里，v2 做了出网、调度、技能沉淀、连接器、浏览器、语音。
v3 补的是剩下的五块，以及 v2 里被诚实标注为「未实现」的两处。

## 五块能力

| # | 能力 | 包 | 关键约束 |
|---|---|---|---|
| A | 多 agent 协作 | `internal/collab` | 一个子 agent = 独立会话 + 独立沙箱 + 独立 ACP 连接；父 run 阻塞等子 run 终态再拿摘要 |
| B | 用户视角行动账本 | `internal/activity` | 聚合事件流 / 审批 / 出网 / 子任务，输出带 status 的时间线 + 「现在卡在哪」 |
| C | egress Ask 审批联动 | `internal/egress` + `internal/runner/egress_approval.go` | 复用现有审批表，键 `(runID, toolCallID)`；egress 不 import runner |
| D | 连接器 proxy 注入 | `internal/connector` + egress sidecar | token 永不进沙箱；HTTPS CONNECT 不解密，走本地明文 sidecar |
| E | 出网强制边界 | `internal/sandbox` | Seatbelt 只放行本机代理端口，域名策略仍由代理执行；必须落 `meta.json` |

## 三个架构决策

### 1. 装配环用 lazy adapter 解开

```
egress 需要  Approver(runner) + TokenProvider(connector)
runner 需要  Egress(egress) + Connectors(connector)
```

让 egress 直接 import runner 会成环（`runner/manager.go` 已 import egress）。
解法不是改接口，而是 app 层传「先占位、后填实」的 `lazyApprover` / `lazyTokenProvider`：
`openEgress` 时是空壳，`runner.New` 与 `openConnectors` 完成后 `Set(...)` 填实。

**空壳必须保守**：审批按 deny、token 按未授权。绝不能因为装配未完成就放行一次出网。

### 2. 依赖倒置的窄接口（`internal/egress/ext.go`）

```go
type Approver interface {
    Request(ctx, ApprovalRequest) (ApprovalResult, error)
}
type TokenProvider interface {
    TokenFor(ctx, tenantID, userID, host string) (header, value string, ok bool, err error)
    BaseFor(name string) (baseURL string, ok bool)
    NameFor(host string) (name string, ok bool)
}
```

egress 保持零内部依赖。app 的 `adapters.go` 把 `runner.Manager` 和
`connector.Manager` 适配过去。

### 3. HTTPS 不能透明注入 token —— 用 sidecar，不做 MITM

CONNECT 隧道里客户端自己跑 TLS，代理只搬密文，看不到 `Authorization`。
透明注入只能 MITM（自建 CA + 双层 TLS + 证书固定），改动与安全面都过大，**这一轮不做**。

改为代理暴露明文端点：

```
http://127.0.0.1:<proxy>/_connector/{name}/<上游路径>
  → 代理按 name 查 TokenFor(tenant, user, host) 注入 header
  → 转发到 Connector.APIBase（https）
```

`connector.Inject` 在 proxy 模式下把 `*_API_BASE` 指向这个地址（**不含任何 token**），
由 app 通过 `Config.SidecarBase` 传入。`InjectEnv` 仍保留明文分支（有泄漏风险，默认关闭）。

**已知代价**：agent 必须显式使用这些地址；官方 SDK 需要能配 base URL。
对 CONNECT 直连的上游 HTTPS，代理既看不到也改不了请求头——这是设计边界，不是 bug。

### 4. 双层出网边界

```
第 1 层（强制，Seatbelt）：(deny network*) + 只放行 localhost:<proxyPort>
                          + DNS 的 unix-socket + 本机 inbound/bind
第 2 层（策略，egress）  ：按域名 allow/deny/ask + 审计 + token 注入
```

Seatbelt 只能按数值 IP/端口匹配，看不到域名（`remote ip` 在 DNS 之后判定），
所以「强制所有流量走代理」由它做，「哪些域名可以出去」由代理做，职责不重叠。

⚠️ 网络策略必须像 `meta.Strict` 一样持久化进 `meta.json`：
`Resume` 只用 `AgentSpec{Command: s.m.Agent}` 重启，不带 Env/新字段，
只放内存会在 pause→resume 后静默丢失。

⚠️ 默认关闭（`ASTRA_NETWORK_LOCK=1` 才开），规则写错的代价是「所有 agent 连不上模型」。

## 文件边界（并行任务）

| 任务 | 独占 | 禁止改 |
|---|---|---|
| A collab | `internal/collab/**`（`collab.go` 冻结） | 其它所有包 |
| B activity | `internal/activity/**`（`activity.go` 冻结）、`web/src/pages/admin/ActivityLedger.tsx`、`web/src/router.ts`、`web/src/pages/admin/AdminLayout.tsx`、`web/src/api.ts` 末尾追加、`web/src/styles.css` 追加 | `web/src/types.ts`（已改）、`web/src/api.ts` 的 EVENT_TYPES 段（已改） |
| C egress | `internal/egress/**`（`egress.go`/`ext.go` 冻结）、`internal/runner/egress_approval.go` | `internal/runner/manager.go`、`approval.go` |
| D connector | `internal/connector/**`（`connector.go` 冻结，`ext_stub.go` 待删） | 其它所有包 |
| E sandbox | `internal/sandbox/**` | 其它所有包 |

主控独占：`internal/api/api.go`、`internal/api/collab_admin.go`、`activity_admin.go`、
`internal/app/**`、`internal/store/types.go`、`internal/runner/manager.go`、
`internal/api/openapi.yaml`、`internal/api/openapi_test.go`、`web/src/types.ts`。

## 能力开关（`/v1/meta`）

新增 `collab` / `activity`。`egress` 的 Ask 与 token 注入跟随 `egress`；
Seatbelt 网络锁跟随 `ASTRA_NETWORK_LOCK`，不单独进 meta。
