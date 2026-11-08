# deploy/

k8s 部署清单与流水线脚本。方案说明见 `../docs/部署与流水线方案.md`。

> ## ⚠️ 本仓库的清单**未经真实集群验证**
>
> 写这份清单的机器上没有 docker、没有 kubectl、没有 kustomize、没有 PG，
> 所以以下这些事情**没有发生过**，不要当成已验证：
>
> - `kubectl apply -k k8s/base` 从未真正执行过（连 `--dry-run=server` 都没跑过）
> - 四个工作负载从未在集群里起来过一次
> - 就绪探针在真实依赖（PG / CubeSandbox / ValKey）下是否变绿，未验证
> - `postgres.yaml`、`migrate-job.yaml` 的 securityContext（只读根文件系统 +
>   fsGroup + PVC）是按惯例写的，没有在 k3s 上试过
> - APISIX / CubeSandbox 的 Helm values 是语义清单，字段名以 chart 实际输出为准
>
> 唯一验证过的是**静态自洽性**：`bash tools/check-deploy.sh`（见文末）。
> 它证明「清单之间不矛盾」，不证明「能部署成功」。

## 目录

```
deploy/
├── k8s/
│   ├── base/                    # astra 服务 + PG + 迁移 + ns + config
│   │   ├── namespace.yaml       # 四个 ns，cube 单独 privileged
│   │   ├── config.yaml          # ConfigMap（非敏感配置，含 store/llm 开关）
│   │   ├── secret.example.yaml  # 四个 Secret 的示例（假值，**不参与 apply**）
│   │   ├── postgres.yaml        # 开发用 PG：StatefulSet + headless Svc + PVC
│   │   ├── migrate-job.yaml     # 建表 Job（在 api 之前跑完）
│   │   ├── schema/              # ⚠️ 迁移 DDL，与 Go 源码逐字节一致（脚本校验）
│   │   ├── astra-api.yaml       # 单副本（单机文件 + 进程内单例，不可扩缩）
│   │   ├── astra-worker.yaml    # StatefulSet，稳定标识参与会话哈希
│   │   ├── astra-sandboxd.yaml  # 2 副本 + Lease 选主
│   │   ├── astra-gateway.yaml   # LLM 网关 + MCP server
│   │   └── kustomization.yaml   # 镜像 tag 在此改写
│   ├── overlays/
│   │   ├── staging/             # gitops-bump.sh 改的就是这里的 newTag
│   │   └── prod/
│   ├── apisix/                  # 入口
│   │   ├── gateway.yaml         # GatewayClass + Gateway（MetalLB VIP）
│   │   ├── routes.yaml          # HTTPRoute + ApisixRoute（SSE/WS）
│   │   └── values-snippet.yaml  # APISIX Helm 关键覆盖项
│   └── cubesandbox/
│       └── values.yaml          # 官方 chart 的覆盖项（语义清单）
├── compose/
│   └── m0.yaml                  # M0 单机环境：不起 k8s，直接 docker compose up
└── ci/
    ├── Dockerfile               # 四个二进制共用
    ├── build.sh                 # 构建 + 推送（可移植）
    ├── gitops-bump.sh           # 改 git 里的 tag，交给 ArgoCD
    ├── smoke.sh                 # 部署后冒烟
    └── .cnb.yml                 # CNB 流水线编排
```

## 配置是怎么进容器的（先看这段，否则改清单会改错地方）

配置分两类，注入方式完全不同，**别混**：

1. **启动参数（flag）**。`cmd/astra-server` 用标准库 `flag` 解析命令行，flag
   本身不读同名环境变量。做法是 **值放 ConfigMap/Secret → 注入成容器 env →
   用 `$(VAR)` 在 args 里展开**，env 纯粹是「参数传递的载体」：

   ```yaml
   env:
     - name: ASTRA_PG_DSN
       valueFrom: { secretKeyRef: { name: astra-db, key: dsn } }
   args:
     - "-dsn=$(ASTRA_PG_DSN)"      # k8s 先替换，再把结果当命令行参数
   ```

   所以「设个 `ASTRA_XXX` 环境变量指望程序自己读」对这类 flag 不生效
   （唯一例外：`-llm-key` 为空时回落读 `DEEPSEEK_API_KEY`）。
2. **可选能力**。记忆 / 观测 / 技能沉淀 / 诊断回灌 / 技能目录 / CLI 路径不是
   flag，程序通过 `os.Getenv` 在 `internal/app/capabilities.go` 等处**直接读**
   （`LANGFUSE_*`、`MILVUS_*`、`DISTILL_AUTO`、`INSIGHT_REPLAY`、`SKILL_DIR`、
   `ASTRA_CLI_BIN`、`ASTRA_AGENT_NAME`…）。这类 env 就是真配置，k8s 里直接以
   字面值或 `valueFrom` 注入容器 env（见 `base/astra-api.yaml` 的「可选能力」块），
   **不进 `$(VAR)`、也不放 ConfigMap 的 flag 载体里**。留空 = 关闭 / 用代码默认值。

| 配置项 | 来源 | 传法 | 说明 |
|---|---|---|---|
| 存储后端 | ConfigMap `store` | `-store=$(ASTRA_STORE)` | `postgres` |
| PG 连接串 | Secret `astra-db/dsn` | `-dsn=$(ASTRA_PG_DSN)` | 含密码，**绝不放 ConfigMap** |
| JWT 密钥 | Secret `astra-auth/jwt-secret` | `-jwt-secret=$(ASTRA_JWT_SECRET)` | 为空则随机生成，重启后 token 全失效 |
| 开 LLM 网关 | ConfigMap `llm-gateway` | `-llm-gateway=$(ASTRA_LLM_GATEWAY)` | bool flag，`true`/`false` |
| 上游地址 | ConfigMap `llm-upstream` | `-llm-upstream=$(...)` | 默认 `https://api.deepseek.com`，不带 `/v1` |
| 上游 key | Secret `astra-llm/api-key` | `-llm-key=$(ASTRA_LLM_KEY)` | 见下面的取舍说明 |
| 沙箱 agent 的 base_url | ConfigMap `llm-public-url` | `-agent-env=OPENAI_BASE_URL=$(...)` | 走 `-agent-env` 转发给 agent 子进程 |

**可选能力 env**（`base/astra-api.yaml` 的「可选能力」块，程序 `os.Getenv` 直读，
不是 flag、不进 `$(VAR)`；留空 = 关 / 用默认，要开就填值或用 overlay 覆盖）：

| env | 默认 | 作用 |
|---|---|---|
| `LANGFUSE_HOST` / `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` | 空（关） | 观测；三项一起填才生效 |
| `MILVUS_ADDR` | 空（关） | 长期记忆；填 Milvus 地址开启 |
| `SKILL_DIR` | `/data/skills` | 技能正文 / `index.json` / `drafts.json` 目录（单机文件） |
| `DISTILL_AUTO` | `true` | 成功 run 自动沉淀 skill 草稿 |
| `INSIGHT_REPLAY` | `true` | 失败诊断回灌下一轮 prompt |
| `ASTRA_CLI_BIN` | 空（自动定位） | 留空找 `<dataDir>/bin/astra-cli` |

密钥类（`LANGFUSE_SECRET_KEY`）别在清单里写明文，生产改用 `valueFrom.secretKeyRef`
或 External Secrets；清单里给的是空占位。

两个刻意的取舍：

1. **真 key 用 `-llm-key` 传，不用 `DEEPSEEK_API_KEY` 环境变量。** 因为
   `resolveAgent` 在 `-agent=dsh` 时会把 `DEEPSEEK_API_KEY` 转发进 agent 子进程，
   也就是**会跟着 agent 进沙箱**，与「真 key 只留控制面」直接冲突。代价是 key
   出现在进程 argv 里——可接受：镜像是 distroless，容器内没有 shell 也没有别的
   进程能读 `/proc/<pid>/cmdline`。
2. **沙箱里的凭据是虚拟 key，不是真 key。** `-llm-public-url` 指向 api 的
   集群内 Service 地址，平台会为每个会话签发 2 小时有效期的虚拟 key，注入成
   `DEEPSEEK_API_KEY` 与 `DEEPSEEK_BASE_URL`。真 key 只在 api 进程内存里。
   ⚠️ 这个地址必须是 **agent 视角**可达的根地址（不带 `/v1/llm` 后缀），
   与 `-addr` 的监听地址不是一回事。
   ⚠️ 早期清单里用 `-agent-env=OPENAI_BASE_URL=...` 传，那是错的：dsh 走的是
   Anthropic Messages 协议，认的是 `DEEPSEEK_BASE_URL`。

## 一次性准备

```bash
# 1) 命名空间
kubectl apply -f k8s/base/namespace.yaml

# 2) 真 Secret（不要提交！示例见 base/secret.example.yaml）
#    真值来源三选一：sealed-secrets / External Secrets / 手敲下面这条
kubectl -n astra-system create secret generic astra-db \
  --from-literal=dsn='postgres://astra:<密码>@postgres.astra-system.svc:5432/astra?sslmode=require' \
  --from-literal=postgres-password='<同一个密码>' \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n astra-system create secret generic astra-auth \
  --from-literal=jwt-secret="$(openssl rand -base64 48)" \
  --from-literal=sandbox-token-secret="$(openssl rand -base64 48)"

kubectl -n astra-system create secret generic astra-llm \
  --from-literal=api-key='<上游真 key>'

kubectl -n astra-system create secret generic astra-cube \
  --from-literal=api-token='<CubeSandbox token>'
```

校验 Secret 齐了没有（缺一个 pod 就 `CreateContainerConfigError`）：

```bash
for s in astra-db astra-auth astra-llm astra-cube; do
  kubectl -n astra-system get secret "$s" >/dev/null || echo "缺 $s"
done
```

## 部署顺序

顺序是有依赖的：`migrate` 必须先于 `api` 能用。但**这个顺序不是靠 apply 的先后保证的**，
而是靠就绪探针 —— `astra-api` 的 `/readyz` 会真去戳 store，表没建好就返回 503，
k8s 不会给它导流量。所以下面这套顺序是安全的，不需要拆成多步 apply：

```bash
# 1) 一次性应用（PG → 迁移 Job → api → worker → 网关，kustomize 一起出）
kubectl apply -k k8s/base

# 2) 等建表 Job 结束（它和 api 进程内部的迁移都是幂等的，谁先谁后都不出错）
kubectl -n astra-system wait --for=condition=complete job/astra-migrate --timeout=300s
#    失败就看日志（最常见：dsn 错、PG 没起、sslmode=require 但库没证书）
#    kubectl -n astra-system logs job/astra-migrate

# 3) 控制面就绪（/readyz 变绿才算）
kubectl -n astra-system rollout status deploy/astra-api --timeout=180s
#    首次启动会打印默认管理员密码（只打印一次，立刻存下来）：
kubectl -n astra-system logs deploy/astra-api | grep -A6 '已初始化默认租户'

# 4) worker
kubectl -n astra-system rollout status statefulset/astra-worker --timeout=300s

# 5) 入口（APISIX 用 Helm 装，命令见下面「APISIX」一节）
kubectl apply -f k8s/apisix/gateway.yaml -f k8s/apisix/routes.yaml

# 6) 冒烟
bash ci/smoke.sh
```

> 为什么用 `kubectl wait` 而不是 ArgoCD 的 sync-wave / Helm 的 hook 注解：
> 本仓库既不用 ArgoCD 的同步编排，也不用 Helm 装 astra 自己的服务（`apply -k` 直上），
> 那些注解在这里没人会读，写了只会让人误以为顺序被保证了。
>
> 为什么迁移不用 initContainer：api 有 3 个副本，initContainer 会并发跑 3 次 DDL
> 抢同一把 PG 的 DDL 锁，冷启动能被拖到几十秒，报错还难懂。独立 Job 只跑一次，
> 失败就停在 `Failed` 等人看。详见 `migrate-job.yaml` 顶部。

**APISIX**（入口，Helm 装，不属于 `apply -k` 的范围）：

```bash
helm repo add apisix https://charts.apiseven.com
helm install apisix apisix/apisix -n astra-system -f k8s/apisix/values-snippet.yaml
kubectl apply -f k8s/apisix/gateway.yaml -f k8s/apisix/routes.yaml
```

**CubeSandbox**（Helm，只落 cube 节点；⚠️ 确认版本 ≥ v0.7.0，
v0.4.0 之前的 eBPF checksum bug 会让同节点访问沙箱端口全不通）：

```bash
helm install cubesandbox <官方 chart> -n astra-cube -f k8s/cubesandbox/values.yaml
```

**生产额外一步**：把 `k8s/base/kustomization.yaml` 里 `- postgres.yaml` 注释掉，
改用外部 RDS（`postgres.yaml` 顶部有完整说明），并把 dsn 换成 `sslmode=require`。

## 构建与发布

```bash
# 本地构建（不推送）
TAG=dev ./ci/build.sh

# CI 构建并推送
REGISTRY=harbor.astra.local/astra TAG=abc1234 PUSH=1 ./ci/build.sh

# 发布到 staging（改 git 里的 newTag，ArgoCD 同步；回滚 = git revert）
./ci/gitops-bump.sh abc1234 staging
```

镜像 tag 的唯一改写入点是 `k8s/overlays/<env>/kustomization.yaml` 的 `newTag`。
base 里写的是 `:latest`——它是**合法可解析**的引用（能通过 kustomize build /
apply 的校验），只是不该拿去上线。临时本地验证可以用：

```bash
kustomize edit set image harbor.astra.local/astra/astra-api:dev   # 别提交
kubectl apply -k k8s/base
```

## 未实现的部分（部署前必读）

清单描述的是**当前可部署的形态**，与代码一一对应。仍然缺的东西如下：

| 缺什么 | 现状 | 影响 |
|---|---|---|
| `astra-worker` / `astra-sandboxd` / `astra-gateway` | `cmd/` 下只有 `astra-server` | 三份清单**已移到 `k8s/future/`**，不参与 apply。留在 base 里只会让 `apply -k` 得到三个 `ImagePullBackOff`，把真问题淹掉 |
| 多副本 | 单机文件 + 进程内单例：`insights.json`（失败诊断）、`index.json`（技能索引）、`drafts.json`（沉淀草稿）、`SKILL_DIR/`（技能正文），以及 `internal/runner/approval.go` 的内存审批注册表 `sync.Map` | 副本数必须保持 1（`astra-api.yaml` 已固定 `replicas: 1`）。多副本各写各的文件、请求随机路由会读不到别的副本写的内容；A 副本挂起的审批，B 副本上的用户点不到。设计文档 §7 明确不做多副本共享（PG 化另排） |
| `ci/build.sh` 的 `BINS=(api worker sandboxd gateway)` | 只对应得上 `astra-server` 一个二进制 | CI 只能产出 `astra-api` 镜像 |
| `astra-worker drain` 子命令（preStop 用） | 不存在 | 与 worker 一起实现 |

拆 worker 的硬前提写在 `k8s/future/README.md` 里——不是「加个 Deployment」那么简单，
前提是审批状态离开进程内存。

> 本轮已修掉的两项：
> - **迁移入口**：`astra-server -migrate-only` 已实现，`migrate-job.yaml` 改为调用它，
>   不再用 `psql` 跑手抄的 DDL 副本（那份副本迟早与代码漂移）。
> - **假端口**：`astra-api` / `astra-worker` 上原本有个 9090 的 metrics 端口，
>   但代码里只有一个 listener（`-addr`），9090 从来没被监听过。已删除，
>   `/metrics` 与业务 API 同端口。

## 验证清单

- [ ] `astra-api` Ready（当前 base 里只有这一个工作负载）
- [ ] `job/astra-migrate` 状态 `Complete`
- [ ] `astra-api /healthz` 集群内可通
- [ ] `astra-api /readyz` 返回 200 且 `checks.store=ok`（PG 连不上会是 503）
- [ ] `astra-api /metrics` 能抓到 `astra_runs_total`（同端口，不是 9090）
- [ ] 经 APISIX `GET /v1/healthz` 200
- [ ] **SSE 3 秒内能收到字节**（收不到就是 buffer 没关）
- [ ] cube-node DaemonSet Running，且 `kubectl -n astra-cube get pod -o wide` 落在 cube 节点
- [ ] 沙箱创建 → 执行 → 销毁全通
- [ ] worker 滚动更新时会话不中断（drain 生效）

## 静态自检

```bash
bash tools/check-deploy.sh     # 失败退出码非 0，可直接挂 CI 的 verify 阶段
```

在没有 kubectl 的机器上能查出来的问题（10 类）：

1. 占位符残留（非法镜像引用，apply 阶段才炸）
2. 每个 YAML 能否被解析
3. 工作负载是否带 `image` / `livenessProbe` / `readinessProbe` / `resources.limits`
4. 探针路径是否 `/healthz` + `/readyz`，端口是否真被声明过
5. 引用的 ConfigMap/Secret 是否存在（只在 example 里存在的会单独提示）
6. kustomization 的 `resources` / `configMapGenerator.files` 是否存在
7. 有无硬编码密码（结构化检查 env/ConfigMap/Secret + 正则兜底）
8. 迁移 DDL 副本与 `internal/store/sqlstore/schema_pg.sql` 是否逐字节一致
9. `astra-api` 的 args 里有没有 main.go 不存在的 flag（写错会 `exit 2`）
10. astra 自有工作负载的 prometheus 注解端口是否真被声明

解析器优先级：`pyyaml` → `ruby/psych` → 降级为 grep 文本断言（会用 ⚠️ 明示）。
本机 `python3` 没有 pyyaml，实际走的是 ruby 分支。

## 三个最容易翻车的地方

1. **SSE 被缓冲**：`ApisixRoute` 里 `proxy-control.request_buffering: false` + Nginx 层 `proxy_read_timeout`，缺一个就是「前端转圈一分钟然后一次性涌出」。
2. **cube 特权 pod 落到业务节点**：nodeSelector/taint 没配，`/dev/kvm` 就暴露在跑业务的机器上。
3. **计算面 pod 设了紧的 limits**：k8s 和 CubeMaster 两套调度器打架，沙箱创建超时。计算面 `limits` 不设或给很大。

## 前置条件（缺一个都起不来）

| # | 条件 | 为什么 |
|---|---|---|
| 1 | k3s 装好，**`--disable traefik`** | 不关的话 APISIX 和 Traefik 抢 80/443 |
| 2 | cube 节点：x86_64 + `/dev/kvm` + `/sys/fs/bpf` | CubeSandbox 是 KVM microVM |
| 3 | cube 节点：**独立 XFS 盘挂 `/data/cubelet`** | 默认 loopback XFS 是试用路径，性能差且随 pod 漂移 |
| 4 | 节点打标签 + taint | `astra.io/cube-node=true` / `NoSchedule`，防特权 pod 落到业务节点 |
| 5 | MetalLB 装好并配 IPAddressPool | 自建集群没有云 LB，不装 Gateway 一直 Pending |
| 6 | Harbor 可拉 | 镜像仓库要在 k3s 的 `registries.yaml` 里配 insecure/CA |
| 7 | 四个 Secret 已存在 | `astra-db` / `astra-auth` / `astra-llm` / `astra-cube` |
| 8 | StorageClass `local-path` | `postgres.yaml` 的 PVC 用它；没有就改 `storageClassName` |

## 依赖组件（一）：向量记忆 Milvus 与观测 Langfuse 的 compose

> 上面那棵树写于本目录只有 base/apisix/compose 的时候。本轮新增：
> `deploy/vector/`、`deploy/observability/`、`deploy/k8s/vector/`、`deploy/k8s/observability/`。
> 树上没画出来，以本节为准。

### 定位：这两套是**可选能力**

缺了它们平台照常启动：`MILVUS_ADDR` 留空就是记忆关闭，`LANGFUSE_HOST` 留空就是观测关闭
（`/v1/meta` 里能看出哪些能力关着）。所以 Milvus / Langfuse **服务本身**不进
`deploy/k8s/base`，也不进 `make serve` 的启动路径——把可选依赖塞进主流程，等于让
「没装 docker」阻塞整个开发流。但 `base/astra-api.yaml` 的 Deployment **已经注入了
这些开关的 env**（默认留空=关，见上面「可选能力 env」表），k8s 部署想开就填值或
由 overlay 覆盖，不再是「无从开启」。

### 服务清单与端口

`deploy/vector/compose.yaml`（project 名 `astra-vector`）：

| 服务 | 镜像（显式 tag） | 宿主端口 | 作用 |
|---|---|---|---|
| `etcd` | `quay.io/coreos/etcd:v3.5.16` | `127.0.0.1:2379` | Milvus 元数据 |
| `seaweedfs` | `chrislusf/seaweedfs:3.71` | `127.0.0.1:8333`（S3）/ `9333`（master） | 代替 MinIO |
| `seaweedfs-init` | 同上 | — | 一次性：建 bucket `milvus` |
| `milvus` | `milvusdb/milvus:v2.4.15` | `127.0.0.1:19530`（gRPC）/ `9091`（metrics+healthz） | 向量库 |

`deploy/observability/compose.yaml`（project 名 `astra-observability`）：

| 服务 | 镜像（显式 tag） | 宿主端口 | 作用 |
|---|---|---|---|
| `postgres` | `postgres:16-alpine` | `127.0.0.1:5433` | Langfuse 业务库 |
| `clickhouse` | `clickhouse/clickhouse-server:24.8` | `127.0.0.1:8123`（HTTP） | trace 存储 |
| `valkey` | `valkey/valkey:8.0` | `127.0.0.1:6380` | 队列 + 缓存 |
| `seaweedfs` | `chrislusf/seaweedfs:3.71` | `127.0.0.1:8334`（S3）/ `9334`（master） | 代替 MinIO |
| `seaweedfs-init` | 同上 | — | 一次性：建 bucket `langfuse` |
| `langfuse-web` | `langfuse/langfuse:3.227.0` | `127.0.0.1:3000` | UI + Public API |
| `langfuse-worker` | `langfuse/langfuse-worker:3.227.0` | — | 消费队列 → 写 ClickHouse |

两套的 SeaweedFS 端口刻意错开（8333/9333 vs 8334/9334），可以同时起。
宿主端口全部绑 `127.0.0.1`：这些组件**没有**一个是面向公网的，绑 0.0.0.0
等于把没有密码的 clickhouse / valkey 挂到局域网。

### 起停

```bash
make deps-up          # 两套一起起
make deps-status      # 逐项探测：谁没起来、该看哪条日志
make vector-up        # 只起 Milvus
make obs-up           # 只起 Langfuse
make deps-down        # 停容器，保留数据
make deps-clean       # 停 + 删数据（要输 yes；FORCE=1 跳过确认）
```

`make deps-status` 不查 `docker ps`，而是**真去探本机回环端口与健康端点**——
因为「容器 Running」和「服务可用」是两件事（milvus 的容器能一直在 Running 而
healthz 永远 503）。没有 docker 时它会把 9 项全报「未起来」并正常退出（exit 0），
这是预期结果。

### ⚠️ 未实机验证（必读）

写这份编排的机器上**没有 docker**（`which docker` → not found），所以：

| 做过 | 没做过 |
|---|---|
| 两份 compose 的 YAML 语法（ruby/psych 解析通过） | `docker compose up` 从未执行 |
| `docker compose config` 的等价物**没有**跑过（需要 docker CLI） | 两套服务从未一起起过 |
| `make -n deps-up` 打印出的命令正确 | 镜像是否能拉（Docker Hub 在本机不可达，tag 存在性未核） |
| `make deps-status` 的「全部未启动」路径 | 探针在真依赖下是否会变绿 |
| `bash tools/check-deploy.sh` 对新 k8s 清单全绿 | ClickHouse 建表、Langfuse 迁移是否跑得通 |

**不要把「静态自检通过」当成「能起来」。** 第一次真跑请按下面的顺序排障。

### 排障顺序（按依赖层次，别跳步）

原则：**下层不通，上层永远起不来**，而且上层的报错通常指向错误的地方
（milvus 会报 etcd 超时，但它真正的问题是 S3 没 bucket）。

**① 基础依赖：etcd / SeaweedFS**

```bash
# etcd
curl -sf http://127.0.0.1:2379/health && echo etcd-ok
# SeaweedFS master
curl -sf http://127.0.0.1:9333/cluster/status
# bucket 是否真建了（milvus 那套）
echo "s3.bucket.list" | docker compose -f deploy/vector/compose.yaml \
  exec -T seaweedfs weed shell -master=127.0.0.1:9333
# 没看到 milvus → 看 init 容器：docker compose ... logs seaweedfs-init
```

**② 数据层：milvus / clickhouse / postgres / valkey**

```bash
curl -sf http://127.0.0.1:9091/healthz && echo milvus-ok        # 这一步绿 = etcd+S3 都通
curl -sf http://127.0.0.1:8123/ping && echo clickhouse-ok
docker compose -f deploy/observability/compose.yaml exec postgres \
  psql -U langfuse -d langfuse -c 'select 1'
docker compose -f deploy/observability/compose.yaml exec valkey valkey-cli ping
```

Milvus 真的能收请求（比 healthz 更强的一步，需要 `brew install grpcurl`）：

```bash
grpcurl -plaintext 127.0.0.1:19530 list
# 期望列出 milvus.proto.milvus.MilvusService
grpcurl -plaintext -d '{"dbName":"default","collectionName":"astra_memory"}' \
  127.0.0.1:19530 milvus.proto.milvus.MilvusService/DescribeCollection
# 集合是应用第一次写入时建的：刚起完这边会返回 collection not found，这是正常的，
# 先用 make serve + MILVUS_ADDR=127.0.0.1:19530 写一条记忆再回来看
```

**③ 应用层：langfuse-web / langfuse-worker**

```bash
curl -sf http://127.0.0.1:3000/api/public/health
# 用自动创建的那对 key 查一次 trace（空列表 = 200 + {"data":[],"meta":...} 也是通的）
curl -s -u pk-lf-astra-dev:sk-lf-astra-dev \
  'http://127.0.0.1:3000/api/public/traces?limit=1'
# clickhouse 里有没有表（没有表 = web 的迁移没跑完）
docker compose -f deploy/observability/compose.yaml exec clickhouse \
  clickhouse-client -u langfuse --password "$OBS_CLICKHOUSE_PASSWORD" -q 'SHOW TABLES FROM langfuse'
```

「UI 能登录但 trace 列表空」的三个常见原因，按概率排：
1. `langfuse-worker` 没起（异步写入没人消费）
2. bucket 不存在或 S3 凭据与 `s3.json` 不一致（上报 207 但对象写不进去）
3. clickhouse 时区不是 UTC（时间范围查询返回空）

### 版本兼容（这是本轮最容易翻车的地方）

| 组件 | 本仓库钉的版本 | Langfuse v3 的要求 | 说明 |
|---|---|---|---|
| Langfuse | `3.227.0`（web + worker 同号） | — | ⚠️ **v3 已被上游标为 deprecated**，v4 已 GA |
| ClickHouse | `24.8` | **≥ 24.3** | v4 的门槛是 25.12+，升 Langfuse 必须同时升 CH |
| PostgreSQL | `16-alpine` | ≥ 12 | v4 建议 ≥ 15 |
| Valkey / Redis | `8.0` | Redis ≥ 7 | v3 时代是 redis 7；valkey 8 协议兼容，**官方只在 v4 文档里明确列为支持** |
| S3 | SeaweedFS 3.71 | 任意 S3 兼容 + path style | `FORCE_PATH_STYLE=true` 必须开 |
| Milvus | `2.4.15` | — | `milvus-sdk-go/v2` 与 2.4 配套；不要跟着上游升 2.6/3.0 |

三个具体风险：

1. **不要单独升 Langfuse。** v3 → v4 换了写入协议（批量 ingestion → OTel 风格），
   `internal/trace/langfuse.go` 的 HTTP 拼装会整体失效；同时 ClickHouse 门槛从 24.3 抬到 25.12。
2. **不要单独升 ClickHouse 到 25.x。** v3 的迁移脚本没在 25.x 上验证过；反过来
   把 Langfuse 升到 v4 又必须升 CH —— 这两件事必须一起做，且要先备份 CH 数据。
3. **SeaweedFS 的所有权模型与 MinIO 不同。** 预签名 URL、`ListObjectsV2` 的分页
   行为在 SeaweedFS 上是自己实现的，Langfuse 的 media 上传（浏览器直连）是最可能
   出问题的地方。trace 上报（服务端到服务端）风险低得多。

### 许可红线（为什么不是 MinIO / redis）

| 被换掉的 | 换成了 | 原因 |
|---|---|---|
| MinIO | SeaweedFS（Apache-2.0） | MinIO 社区版 AGPLv3 |
| redis ≥ 7.4 | valkey 8（BSD-3-Clause） | redis 7.4 起 RSALv2/SSPLv1 |

SeaweedFS 的 `identities` 一旦非空就拒绝匿名请求；空 identities 时它**默认放行匿名读写**，
所以 `deploy/*/seaweedfs/s3.json` 里必须配 identities（现在配了开发用 key）。
生产把那两个 key 换成 Secret 挂载，别提交真值。

### 要覆盖默认值时改哪里

两套 compose 的所有可调项都是**环境变量 + 内置默认值**（默认值是开发用的，能直接跑）。
覆盖方式（优先级从高到低）：

```bash
export OBS_PG_PASSWORD=...        # 1) 当前 shell
# 2) deploy/vector/.env  /  deploy/observability/.env —— compose 默认会读「项目目录」的 .env
#    （.gitignore 里的 `.env` 规则对任意层级生效，不会被提交）
# 3) 直接改 compose 里的默认值（不推荐：一次本地改动容易跟着提交上去）
```

| 变量 | 默认 | 用途 |
|---|---|---|
| `MILVUS_S3_ACCESS_KEY` / `MILVUS_S3_SECRET_KEY` / `MILVUS_S3_BUCKET` | `astra-dev-key` / `astra-dev-secret-change-me` / `milvus` | Milvus 连 SeaweedFS 的凭据，**必须与 `deploy/vector/seaweedfs/s3.json` 一致** |
| `OBS_PG_USER` / `OBS_PG_PASSWORD` / `OBS_PG_DB` | `langfuse` / `langfuse-dev-password` / `langfuse` | Langfuse 业务库 |
| `OBS_CLICKHOUSE_USER` / `OBS_CLICKHOUSE_PASSWORD` / `OBS_CLICKHOUSE_DB` | 同上 | trace 库 |
| `LANGFUSE_S3_ACCESS_KEY` / `LANGFUSE_S3_SECRET_KEY` / `LANGFUSE_S3_BUCKET` | `langfuse-dev-key` / … / `langfuse` | 事件与媒体 bucket，须与 `deploy/observability/seaweedfs/s3.json` 一致 |
| `LANGFUSE_NEXTAUTH_SECRET` / `LANGFUSE_SALT` / `LANGFUSE_ENCRYPTION_KEY` | 开发占位值 | 会话签名 / key 哈希盐 / 字段加密；**生产必须换**，`ENCRYPTION_KEY` 要 64 位 hex |
| `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` | `pk-lf-astra-dev` / `sk-lf-astra-dev` | 首启自动建的那对 key（要填进 `.env.example` 的同名变量） |
| `LANGFUSE_MEDIA_ENDPOINT` | `http://127.0.0.1:8334` | 浏览器访问附件的地址（预签名 URL 的 host） |

`LANGFUSE_ENCRYPTION_KEY` 换了之后**已加密的旧数据解不开**（比如已有的集成凭据），
换之前先确认库里没有需要保留的加密字段。

### 数据卷

```
deploy/vector/data/{etcd,seaweedfs,milvus}
deploy/observability/data/{postgres,clickhouse,clickhouse-logs,valkey,seaweedfs}
```

已加进 `.gitignore`。`deps-down` 不动它们，`deps-clean` 才删。

### 静态校验（没有 docker 时唯一能做的）

```bash
# YAML 合法性（本机没有 pyyaml，用 macOS 自带 ruby）
ruby -ryaml -e 'YAML.load_stream(File.read("deploy/vector/compose.yaml")); puts "ok"'
ruby -ryaml -e 'YAML.load_stream(File.read("deploy/observability/compose.yaml")); puts "ok"'

# 镜像 tag 必须显式（不接受 latest）
grep -nE '^\s+image:' deploy/vector/compose.yaml deploy/observability/compose.yaml
```

## 依赖组件（二）：k8s 清单（vector / observability）

### 文件

```
deploy/k8s/vector/
├── kustomization.yaml
├── namespace.yaml          # astra-data（与 base/namespace.yaml 内容一致的自足副本）
├── etcd.yaml               # StatefulSet + headless Svc + PVC(5Gi)
├── seaweedfs.yaml          # Deployment(Recreate) + Svc + PVC(50Gi)
├── bucket-init.yaml        # Job：建 bucket milvus
├── milvus.yaml             # StatefulSet + Svc(19530/9091) + PVC(20Gi)
└── secret.example.yaml     # vector-s3（access-key / secret-key / s3.json）

deploy/k8s/observability/
├── kustomization.yaml
├── namespace.yaml          # astra-ops
├── postgres.yaml           # StatefulSet + headless Svc + PVC(20Gi)
├── clickhouse.yaml         # StatefulSet + Svc(8123/9000) + PVC(50Gi+5Gi 日志)
├── valkey.yaml             # Deployment(Recreate) + Svc + PVC(5Gi)
├── seaweedfs.yaml          # Deployment + Svc + PVC(50Gi) + Job(建 bucket)
├── langfuse.yaml           # ConfigMap + Deployment(web) + Deployment(worker) + Svc(3000)
└── secret.example.yaml     # langfuse-db / langfuse-secret / langfuse-s3
```

两个命名空间是**复用**已有的分层（`astra-data` / `astra-ops`），不是新造的。

### 用之前

```bash
kubectl apply -k deploy/k8s/vector
kubectl apply -k deploy/k8s/observability

# 先看基础设施（三个 StatefulSet 都要 Ready）
kubectl -n astra-data get sts,pod
kubectl -n astra-ops  get sts,deploy,pod
# bucket Job 必须是 Complete（否则 milvus / langfuse 起来后会一直报 no such bucket）
kubectl -n astra-data wait --for=condition=complete job/vector-bucket-init --timeout=300s
kubectl -n astra-ops  wait --for=condition=complete job/langfuse-bucket-init --timeout=300s
# milvus 就绪
kubectl -n astra-data rollout status sts/milvus --timeout=600s
# langfuse 就绪（先 web 再 worker：worker 依赖 web 跑完 clickhouse 迁移）
kubectl -n astra-ops rollout status deploy/langfuse-web --timeout=600s
kubectl -n astra-ops rollout status deploy/langfuse-worker --timeout=300s
```

Secret 必须先建（清单里的 `secret.example.yaml` **不参与 apply**，只说明需要哪些键）：

```bash
kubectl -n astra-data create secret generic vector-s3 \
  --from-literal=access-key='...' --from-literal=secret-key='...' \
  --from-file=s3.json=./s3-rendered.json
# langfuse-db / langfuse-secret / langfuse-s3 的命令见 observability/secret.example.yaml 顶部
```

### 与 compose 的差异（别照着 compose 的端口去 k8s 里找）

| 项 | compose | k8s |
|---|---|---|
| 对外暴露 | 宿主回环端口 | 只有 ClusterIP，**没有 Ingress** |
| 存储 | `./data/<svc>` | `local-path` PVC |
| 副本 | 1 | 1（clickhouse/milvus/pg 都是单点） |
| 依赖顺序 | `depends_on` + healthcheck | 探针 + 探针导致的 CrashLoop 重试 |

**k8s 侧的已知缺口**（不要以为已经能上生产）：

1. `LANGFUSE_S3_MEDIA_UPLOAD_ENDPOINT` 指的是集群内地址 → **UI 里点开附件会 404**。
   要修得先决定入口方案（APISIX 的路由 + 一个能转发到 `langfuse-seaweedfs:8333`
   的规则），那是独立的一件事，硬编一个域名只会让 apply 失败。
2. `LANGFUSE_HOST` 等可选能力现在**有接入点了**：`base/astra-api.yaml` 的
   「可选能力」env 块直接注入 `LANGFUSE_*` / `MILVUS_ADDR` 等（程序走
   `os.Getenv`），默认留空=关、填上即开。剩下的限制是值本身：集群内 Langfuse /
   Milvus 要先用 `deploy/k8s/observability/`、`deploy/k8s/vector/` 部署好，
   且密钥类应改用 `valueFrom.secretKeyRef` / ESO，别在清单里写明文。
3. 没有 NetworkPolicy：`astra-ops` 里的 clickhouse/valkey 对外是「集群内谁都能连」。
4. clickhouse 单副本 = 数据只有一个节点上的一份。要正经用，先上 ClickHouse Operator
   （Langfuse 官方推荐的形态），而不是给这个 StatefulSet 加 replicas。
5. 本目录清单与上一节一样：**未经真实集群验证**（本机没有 kubectl）。
