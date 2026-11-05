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
│   │   ├── astra-api.yaml       # 无状态，3 副本
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

`cmd/astra-server` 是**纯 flag 程序**：用标准库 `flag` 解析，**不读任何环境变量**
（唯一的例外是 `-llm-key` 为空时会回落读 `DEEPSEEK_API_KEY`）。

所以清单里没有「设个 `ASTRA_XXX` 环境变量让程序自己读」这种写法——那样写了也不生效。
实际做法是：**值放 ConfigMap/Secret → 注入成容器 env → 用 `$(VAR)` 在 args 里展开**，
把 env 纯粹当作「参数传递的载体」：

```yaml
env:
  - name: ASTRA_PG_DSN
    valueFrom: { secretKeyRef: { name: astra-db, key: dsn } }
args:
  - "-dsn=$(ASTRA_PG_DSN)"      # k8s 先替换，再把结果当命令行参数
```

| 配置项 | 来源 | 传法 | 说明 |
|---|---|---|---|
| 存储后端 | ConfigMap `store` | `-store=$(ASTRA_STORE)` | `postgres` |
| PG 连接串 | Secret `astra-db/dsn` | `-dsn=$(ASTRA_PG_DSN)` | 含密码，**绝不放 ConfigMap** |
| JWT 密钥 | Secret `astra-auth/jwt-secret` | `-jwt-secret=$(ASTRA_JWT_SECRET)` | 为空则随机生成，重启后 token 全失效 |
| 开 LLM 网关 | ConfigMap `llm-gateway` | `-llm-gateway=$(ASTRA_LLM_GATEWAY)` | bool flag，`true`/`false` |
| 上游地址 | ConfigMap `llm-upstream` | `-llm-upstream=$(...)` | 默认 `https://api.deepseek.com`，不带 `/v1` |
| 上游 key | Secret `astra-llm/api-key` | `-llm-key=$(ASTRA_LLM_KEY)` | 见下面的取舍说明 |
| 沙箱 agent 的 base_url | ConfigMap `llm-public-url` | `-agent-env=OPENAI_BASE_URL=$(...)` | 走 `-agent-env` 转发给 agent 子进程 |

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
| 多副本 | 审批注册表在内存（`internal/runner/approval.go` 的 `sync.Map`） | 副本数必须保持 1。A 副本挂起的审批，B 副本上的用户点不到 |
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
