# 平台能力扩展：可观测 / 向量记忆 / CLI / Skills / 运营页

> 本轮扩展的**唯一契约**。五个并行任务都按这份文档实现，接口与文件边界不得自行变更。

## 一、模块与文件归属

| 模块 | 负责 | 独占文件 | 禁止改动 |
|---|---|---|---|
| A 可观测（Langfuse） | agent-obs | `internal/obs/**`、`internal/api/obs.go` | `api.go`、`go.mod`、`web/**` |
| B 向量记忆（Milvus） | agent-memory | `internal/memory/**`、`internal/api/memory.go` | `api.go`、`go.mod`、`web/**` |
| C CLI + Skills | agent-cli | `cmd/astra-cli/**`、`cmd/admin-cli/**`、`internal/client/**`、`skills/**` | `api.go`、`go.mod`、`internal/api/**` |
| D 运营页 | agent-web | `web/src/pages/admin/**`、`web/src/styles.css`(追加段)、`web/src/api.ts`(追加函数) | `internal/**`、`cmd/**` |
| E 部署编排 | agent-deploy | `deploy/observability/**`、`deploy/vector/**`、`Makefile`(追加 target)、`third_party/upstream.lock.toml`(追加条目) | `internal/**`、`cmd/**`、`web/**` |

`internal/api/api.go`、`go.mod`、`internal/store/types.go` 由主控独占——需要新增路由/依赖/字段时**不要自己改**，在汇报里列出「需要主控加的几行」，由主控统一添加。

## 二、依赖选型（已定，不再讨论）

| 用途 | 选型 | 理由 |
|---|---|---|
| 观测 | Langfuse（MIT 核心）作为 submodule | 与项目「源码可审计」一致；不用其官方 compose，自己写编排 |
| 观测数据写入 | Langfuse Ingestion HTTP API（`POST /api/public/ingestion`） | 只依赖 net/http，不引 SDK |
| 向量库 | Milvus 2.4（Apache-2.0），Go SDK `milvus-sdk-go/v2` | 已加入 go.mod |
| Milvus 依赖 | etcd（元数据）+ SeaweedFS（对象存储） | **不用 MinIO**：AGPLv3，项目红线 |
| CLI | Go + cobra | 单二进制分发；两个 CLI 共享 `internal/client` |
| Embedding | 可插拔 `Embedder` 接口 | 默认 `openai-compatible`（任意 /embeddings 端点）；无 key 时用 `hash`（仅开发，明确告警） |

## 三、API 契约（主控已在 api.go 注册，handler 由 A/B 实现）

### 记忆（用户维度，强制按 user_id 过滤）

```
POST /v1/memory/ingest        body: {kind:"fact"|"conversation", text, session_id?, meta?}
GET  /v1/memory/search?q=&k=10&kind=      → {items:[{id,text,score,kind,created_at,meta}]}
GET  /v1/memory/stats                     → {enabled,count,collection,dim}
DELETE /v1/memory/{id}                    → 204（合规删除）
```

**安全不变式**：检索与删除必须带上调用者的 `tenant_id + user_id` 作为过滤表达式的一部分，**任何情况下不允许跨用户返回**。admin 也只能按显式指定 user 查询，不能全库扫。

### 可观测

```
GET  /v1/admin/obs/config      → {enabled, endpoint, public_key, dashboard_url}
GET  /v1/admin/obs/traces?run_id=&limit=   → {items:[...]}（代理查询，服务端持 secret key）
```

### 运营（已有 admin 能力复用，不新造）
租户/用户/配额/审计/沙箱/生命周期沿用 `/v1/admin/*` 与 `store.AdminStore`。

## 四、数据模型

### Milvus collection: `astra_memory`

| 字段 | 类型 | 说明 |
|---|---|---|
| id | VarChar(64), primary | ULID 或 sha256 前缀 |
| tenant_id | VarChar(64) | **partition key**（多租户物理隔离） |
| user_id | VarChar(64) | 过滤必带 |
| session_id | VarChar(64) | 可空 |
| kind | VarChar(16) | fact / conversation |
| text | VarChar(8192) | 原文 |
| vector | FloatVector(dim) | 维度由 Embedder 决定，建集合时固定 |
| created_at | Int64 | unix 秒 |
| meta | JSON | 可选附加 |

索引：`IVF_FLAT` + `L2`（dim<1024）或 `HNSW`；搜索时 `expr = tenant_id == "..." and user_id == "..."`。

### 自动入库
订阅现有事件 Hub：run 结束（`run.completed` / `run.cancelled`）时，把该 run 的用户输入与最终回复的摘要写入 `kind=conversation`。**不做逐 token 存储**（体积与信噪比都不划算）。

## 五、CLI 命令表

### `astra-cli`（用户）
```
astra-cli login --server URL            # 存 token 到 ~/.astra/cli.json
astra-cli whoami
astra-cli session list|new|rm
astra-cli run "指令" [--session id] [--wait]     # 提交并可选等待终态
astra-cli run logs <run-id>                      # 跟随事件流
astra-cli run cancel <run-id>
astra-cli files ls|cat|download <session>
astra-cli approve <run-id> <tool-call-id> --allow|--deny
astra-cli memory search "..." --k 10
astra-cli memory add "..." --kind fact
astra-cli memory stats
astra-cli skills list|show|install <name>
```

### `admin-cli`（运营）
```
admin-cli login --server URL --as admin
admin-cli tenant list|create|suspend
admin-cli user list|create|reset-password --tenant
admin-cli quota show|set --tenant
admin-cli key list|rotate
admin-cli sandbox list|kill
admin-cli audit tail [--follow]
admin-cli logs egress|history|activity|children|approvals|usage|billing
admin-cli obs status|traces --run
admin-cli memory stats [--tenant]       # 只读统计，不含内容
```

输出约定：默认人类可读表格；`--json` 输出原始 JSON（给脚本/agent 用）；退出码 0/1/2（2=用法错误）。

## 六、Skills 格式（`skills/<name>/SKILL.md`）

```markdown
---
name: astra-memory
description: 检索用户历史记忆与事实，用于回答「上次那个方案」类问题
version: 1
requires: [astra-cli]
---
# 何时使用
...
# 怎么用
```bash
astra-cli memory search "$QUERY" --k 8 --json
```
# 注意
- 只返回当前用户的记忆，不要尝试指定别的 user_id
```

首批三个 skill：`astra-memory`（记忆检索）、`astra-run`（提交任务并等待）、`astra-files`（工作区文件读写）。

## 七、部署产物

```
deploy/observability/   compose.yaml（langfuse + clickhouse + valkey + postgres）
deploy/vector/          compose.yaml（milvus + etcd + seaweedfs）
deploy/k8s/             追加 obs/ vector/ 的 manifest
Makefile: deps-up / deps-down / obs-up / vector-up / deps-status
```

统一约束：所有 compose 用**显式镜像 tag**（不用 latest），数据卷落在 `deploy/<模块>/data/`（已 gitignore），并给 `make deps-status` 一个「谁没起来」的汇总。

## 八、验收

1. `go build ./...`、`go vet`、`go test ./...` 绿；前端 `tsc -b` + `npm test` 绿
2. 每模块至少一条**真跑通**的证据（不是 mock）：记忆写入→检索命中；Langfuse 收到一条 trace；CLI 对真控制面执行成功；运营页拿到真实数据
3. 没有 Milvus/Langfuse 时平台**照常可用**（能力关闭而非启动失败），`/v1/meta` 里能看出哪些能力关着
4. 所有新配置走环境变量，`.env.example` 补齐并注释
5. 不新增 AGPL/SSPL/BSL 依赖
