# 尚未实现的组件

这里的清单是**架构前瞻**，不参与 `kustomize build ../base`，`cmd/` 下也没有
对应的二进制。放进来是为了保留设计意图与已想清楚的部分，而不是让人以为
`apply -k` 之后它们会工作。

| 清单 | 组件 | 为什么还没拆 |
|---|---|---|
| `astra-worker.yaml` | 跑 agent 的 worker | 与 api 之间缺少 run 派发协议；当前 api 单进程兼任 |
| `astra-sandboxd.yaml` | 沙箱守护（集群内 CubeSandbox 的管理面） | `internal/sandbox/cube.go` 尚未在真实 x86_64+KVM 环境验证 |
| `astra-gateway.yaml` | worker 的出网代理 | LLM 网关已内建在 api 里（`/v1/llm/*`），独立网关只在需要按节点收敛出网时才有意义 |

拆分的**硬前提**（任一不满足就会退化成「多副本互相看不见对方状态」）：

1. 审批注册表从内存迁到 PG —— 现在 `internal/runner/approval.go` 的注册表是
   `sync.Map`，多副本时 A 副本挂起的审批，B 副本上的用户点不到。
2. 事件 append 的 seq 分配已经是事务级（`sqlstore` 里用 `RETURNING` 做的），
   这一点已就绪。
3. worker 的心跳与回收：`internal/lifecycle` 现在只回收沙箱，还没有
   「worker 失联 → 接管其沙箱」的路径。
