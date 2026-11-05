# 0008. 沙箱后端做成可替换的 provider，本地用 Seatbelt + APFS 克隆

- **状态**：已采纳
- **日期**：2026-10-29
- **相关**：`internal/sandbox/`、`cmd/astra-sandbox`、[ADR-0002](0002-agent-in-sandbox-with-acp-proxy.md)

## 背景

M0 用宿主进程直接跑通了 ACP 全链路，证明「协议层不依赖沙箱」。但 M1 要写的 worker
必须持有沙箱句柄、拿数据面端点、做 failover —— 一旦这些代码只对着 CubeSandbox 写，
开发机（Apple Silicon macOS）就再也跑不起完整链路，每次改动都得连远端 x86_64 机器验证。

用户的要求很直接：**沙箱能不能在 Mac 上跑，这样本地就能调试。**

事实核查的结果是「部分能，而且不需要硬来」：

| 方案 | Mac 可行性 |
|---|---|
| CubeSandbox | ❌ 需要 x86_64 + `/dev/kvm`。macOS 的 Hypervisor.framework 不是 KVM，PVM 兜底也只有 x86_64 |
| e2b 官方自托管 | ❌ 同上（Firecracker），且要求 GCP/AWS + Nomad/Consul |
| Apple `container`（macOS 26） | ✅ 每容器一个轻量 Linux VM，`kern.hv_support=1` 已满足。但需装签名 pkg（要管理员密码） |
| 宿主机进程 + `sandbox-exec` | ✅ 立刻可用，且 Seatbelt 是内核级约束 |
| 共享内核容器（Docker Desktop / colima） | ⚠️ 能跑，但要常驻 VM、本机 16GB 偏重，且隔离强度不如前两者 |

## 决策

### 1. 上层只认 `sandbox.Provider`，不认具体后端

```go
type Provider interface {
    Name() string
    Create(ctx, CreateRequest) (Sandbox, error)
    Acquire(ctx, id) (Sandbox, error)   // failover：接管已存在的沙箱
    Destroy(ctx, id) error              // 不要求活着，专治残留
    List(ctx) ([]Info, error)
}
```

接口只覆盖我们真正用到的能力（起、连、快照、暂停、销毁、接管），**不照抄 e2b 的 API 面**。
PTY、端口暴露、文件 RPC 等到 M2 需要时再加，避免现在猜错形状。

`Destroy` 与 `Sandbox.Kill` 分开是踩出来的：已 stopped 的沙箱 `Acquire` 返回
`ErrNotRunning`、拿不到句柄，于是清理逻辑只能「打印成功但目录还在」。这是个假成功，
必须由接口层面消灭。

### 2. 三个实现，各自守住一段场景

| provider | 隔离强度 | 用途 | 快照 |
|---|---|---|---|
| `local` | 文件系统写限制（Seatbelt），**无内核隔离** | 开发调试主线，零安装 | APFS clonefile，毫秒级 |
| `applecontainer` | 每容器独立内核（轻量 VM） | 本地真隔离验证 | 不支持（明确报 `ErrUnsupported`） |
| `cube` | KVM microVM | 生产 | 走模板构建 |

### 3. `local` 用 Seatbelt 而不是「靠自觉」

`local` 起 acp-proxy 时在外面套 `sandbox-exec -p <profile>`，profile 是
「默认全允许、掐掉所有写，再显式放行 workspace / home / tmp」。
这正是 dsh 三档 preset 里 `workspace-write` 的语义。

实测（本机 macOS 26.2）：agent 进程尝试写 `$HOME/ASTRA_ESCAPED.txt` 被拒
（`Operation not permitted`），写 workspace 内的相对路径成功。**这是内核级约束，不是约定。**

为什么是「只掐写」而不是最小权限：agent 是 Node 进程，要读系统库、加载 `node_modules`、
连 LLM。读全放开的代价是可接受的信息暴露，而写限制恰好挡住最危险的一类事故
（agent 改坏宿主文件）；反过来如果把读也收紧，会天天为了「某个库加载不到」浪费时间。

顺带验证了一个容易踩的点：**V8 的 JIT 在 Seatbelt 下正常工作**，不需要额外放行
executable memory。如果这里不通，`local` 就只能退化成无隔离的进程。

### 4. `local` 的快照用 APFS clonefile

`cp -c` 是写时复制：实测 50MB 目录克隆 **10ms**，且几乎不占额外空间。

这让本地可以随手打快照当模板用，不需要镜像构建那一整套。生产的模板治理（版本、
审批、分发）仍然要用 CubeSandbox 的 cubemastercli，两者不在同一层，不要混淆。

### 5. pause/resume 的跨 provider 契约：只保证目录不丢

- CubeSandbox 的 pause 是**内存快照**（进程树冻结，恢复后内存原样）
- `local` 的 pause 只是杀进程，**内存态会丢**，靠 `DSH_HOME`（挂在沙箱目录下的 `home/`）
  里的会话持久化 + `session/resume` 恢复

所以上层不能假设「pause 之后 agent 的内存还在」。能假设的只有：
**workspace 与 home 不丢，endpoint 不变。**

endpoint 不变这条是刻意保证的：`Create` 在已有目录时会**在原端口上重启**，
而不是重新分配端口。否则会出现「生产能恢复、本地恢复不了」的假性问题。

## 实测结果（2026-10-29，Mac mini M4 / macOS 26.2 / arm64）

| 验证项 | 结果 |
|---|---|
| `sandbox-exec` 写限制 | ✅ 宿主写入 `Operation not permitted`，workspace 写入成功 |
| Node V8 JIT 在 Seatbelt 下 | ✅ 正常（3000 万次循环无异常） |
| APFS clonefile | ✅ 50MB 目录克隆 10ms |
| `astra-sandbox up` → ACP 握手 | ✅ `protocolVersion=1`，capabilities 与 M0 一致 |
| `session/new` 经沙箱 | ✅ 返回 sessionId |
| `pause` → `up` → 端点 | ✅ 端口不变（41146 → 41146） |
| `pause` → `up` → 旧会话 `resume` | ✅ 恢复成功（靠 home/ 持久化） |
| 幂等 `up` | ✅ 同一 PID 接管，不重建 |
| `rm` 对已停沙箱 | ✅ 目录真删（修掉了之前的假成功） |

## 后果

### 好的

- **M1 的 worker 可以在 Mac 上完整开发**，不用每次连远端 x86_64
- 换 provider 不改上层：`-provider local|applecontainer|cube` 一个参数的事
- 本地就能验证 failover（`Aquire`）、残留清理（`Destroy`）、端点稳定性
- 快照几乎免费，让「模板/分支」类需求在本地也能试

### 坏的 / 必须知道的

- **`local` 绝对不能跑不可信代码**。没有内核隔离、没有网络策略、没有资源配额。
  客户自传算法必须走 `cube`。这一点写进了包注释，不要绕过。
- `applecontainer` 是 **arm64**，生产是 **x86_64**。本地跑通不代表 x64 镜像没问题。
- `cube` provider **尚未在真实 CubeSandbox 上验证**（没有 x86_64 机器）。
  端点形状按 v0.7.x 的 e2b 兼容 REST 面写的，首次启用前必须跑 `doctor` 核对。
- `applecontainer` 需要用户手工装签名 pkg（要管理员密码），
  所以它不能是默认值；默认必须是零安装的 `local`。

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| 把 CubeSandbox 搬上 Mac | 需要 `/dev/kvm`，macOS 没有；PVM 宿主内核兜底只支持 x86_64 |
| 自建 e2b-local 兼容网关 | 保留 e2b SDK 调用方式对本项目没有收益——worker 是我们自己写的，直接调 provider 接口更短。绕一层只会在调试时多一个故障点 |
| Docker Desktop 当默认 | 常驻 VM 吃内存（本机 16GB）；且共享内核，隔离强度低于 Apple container 与 microVM |
| 只做 local，不做抽象 | M1 一接真沙箱就得重写，且「本地过、线上挂」的差异会被推迟到最后才暴露 |
| 让 local 静默降级为无隔离 | 必须显式可见：`local` 就是没隔离，用了要知道 |
