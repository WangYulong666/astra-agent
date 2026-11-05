# 0005. k3s + APISIX 作为编排与入口

- **状态**：已采纳
- **日期**：2026-10-28
- **相关**：`docs/部署与流水线方案.md`、`deploy/k8s/`

## 背景

需要确定：控制面怎么编排、北向流量怎么进来。约束是既有的**「全套源码可控自部署」红线**——不依赖云厂商托管服务，也不用任何厂商的 Self-Hosting 编排。

## 决策

**编排用 k3s，入口用 APISIX。**

| 组件 | 许可 | 说明 |
|---|---|---|
| k3s | Apache 2.0 | 单二进制，组件全开源。**必须 `--disable traefik`**，否则和 APISIX 抢 80/443 |
| APISIX | Apache 2.0 | k8s 下走 Ingress Controller + Gateway API / CRD |
| ArgoCD | Apache 2.0 | GitOps 同步，CI 不持有 kubeconfig |
| Harbor | Apache 2.0 | 镜像仓库自建 |

需要过等保时换 RKE2（同属 Rancher 生态，迁移成本低）。

### 路由分两种，不是一种

| 流量 | 资源类型 | 原因 |
|---|---|---|
| 普通 REST | `HTTPRoute`（Gateway API） | 标准、可移植 |
| **SSE / WebSocket** | `ApisixRoute` | HTTPRoute 表达不了 APISIX 插件，而 SSE **必须**用插件关缓冲 |

### SSE 的两个必设项，缺一个就废

这是几乎必踩的坑，表现是**「前端转圈一分钟，然后事件一次性涌出来」**：

```nginx
proxy_buffering off;
proxy_read_timeout 3600s;
```

关键：`ApisixRoute` 的 `proxy-control` 插件**只能关 `request_buffering`**，**读超时必须落在 Nginx 层**（`apisix/values-snippet.yaml` 里的 `httpConfig`）。只配 CRD 不配 Nginx，60 秒后连接照断。

### CubeSandbox 在 k8s 上的真实情况

官方 v0.7.0 有 Helm Chart，但有几个必须知道的前提：

| 项 | 事实 |
|---|---|
| 计算面形态 | **privileged DaemonSet（Big Pod）**，hostPath 挂 `/dev/kvm`、`/data/cubelet` |
| 硬件 | x86_64 + KVM；**独立 XFS 盘**（默认 25GiB loopback 只是试用路径，reflink 依赖真盘） |
| 成熟度 | 官方标记 **Preview**：驱逐、升级会中断沙箱；cube-node pod 重建导致网络中断 |
| 调度 | 双调度器冲突——计算面 pod 的 `resources.limits` 要给足或**不设**，否则 Cube 的 overcommit 会被 cgroup 卡死 |

`deploy/k8s/cubesandbox/values.yaml` 里记了四个已踩过的坑：eBPF checksum（v0.4.0 前全坏，低于 v0.7.0 别用）、cubelet 杀父进程变孤儿撞 PID 1（用 tini）、CubeVS 默认拒绝网段覆盖 k8s pod/service CIDR（显式 `allow_out`）、以及上面的 cgroup 限制。

⚠️ **该 values 文件给的是「语义清单」**——说明该覆盖哪些项、为什么，**不是猜出来的 chart 字段名**。实际 `helm install` 前必须跑 `helm show values` 对一遍字段，否则会报 `unknown field`。

## 后果

### 好的

- 全链路许可合规，没有一处需要买 license 或依赖云
- 四服务（api / worker / sandboxd / gateway）本来就是无状态 + PG 有状态，天然适配 k8s
- CI 只改 git 里的 image tag，集群里跑的每个版本都对应一个 commit，回滚是 `git revert`

### 坏的

- **CubeSandbox on k8s 是 Preview**：生产要接受「节点维护会中断沙箱」这个前提，靠预热池 + 会话重建兜底
- **节点池不能混部**：计算面需要 KVM + XFS 直盘，必须独立节点池并打 taint，不能和普通业务混跑
- **运维面变大**：k3s + APISIX + ArgoCD + Harbor + CubeSandbox 五套东西，比单机 compose 复杂一个量级

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| TKE / ACK 等托管 k8s | 直接违反「不依赖云厂商托管」红线。合规审查会问「控制面跑在哪」，答「腾讯云/阿里云托管」就出局 |
| Traefik / Nginx Ingress 做入口 | APISIX 插件体系更完整（限流、鉴权、灰度、审计），且同样 Apache 2.0。注意 k3s 自带 traefik 必须禁用 |
| 只上单机 Docker Compose | 一期可以（M0 就是这么做的），但 worker 需要按会话数水平扩，单机撑不住 |
| **M0 就上 k8s** | 否掉。CubeSandbox on k8s 的节点预检（KVM / XFS reflink / bpffs / CIDR 冲突）+ node-init 至少 3 天，而这 3 天**不产生任何协议层结论**。M0 的目标是验证「沙箱能不能跑 dsh、ACP 能不能驱动」，compose 两小时能起来 |
