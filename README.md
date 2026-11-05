# astra-agent

[![ci](https://github.com/WangYulong666/astra-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/WangYulong666/astra-agent/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![go](https://img.shields.io/badge/go-1.27%2B-00ADD8.svg)](go.mod)

通用 Agent 工作台：用户在 Web / 桌面 / App 上下指令，**Agent 在独立微虚拟机沙箱里干活**，产出物回传，人能随时审批和接管。

> **当前状态：0.2.0 可用级交付——有能双击安装的桌面 App（macOS），Web/PWA 共用一套控制面。**
> 桌面端：首启向导建号+配 key、审批系统通知、关窗后台运行；平台层：租户/认证/计费/监控/OpenAPI/审批持久化全在。
> 详细安装使用看 [docs/使用说明.md](docs/使用说明.md)，构建看 `./scripts/build-app.sh`。
> 缺的是：PG 已真机验证（PostgreSQL 16.4，向导→run 全链路+幂等迁移）；CubeSandbox 已有契约测试+doctor 连通探测（真集群需 x86_64+KVM 宿主，Apple Silicon 物理不可行）；公证脚本已备好（需付费开发者账号）。

![工作台](docs/images/workbench-real.png)

左边是会话语义（一会话 = 一沙箱 = 一个 agent 进程），中间是事件流折叠出来的对话——
工具调用、审批结论、agent 输出各归各位，右边是沙箱工作区的产物。上面这张是
**真实 DeepSeek 模型**写脚本、跑脚本留下的记录。

想拦 agent 就把它停在半路。下面就是模型想把文件写到工作区外面时被沙箱拦住、
申请扩权、停在等人点「允许」的那一刻（真实模型，不是演示桩）：

![审批](docs/images/approval-real.png)

---

## 新成员从这里开始（5 分钟跑起来）

```bash
git clone git@github.com:WangYulong666/astra-agent.git && cd astra-agent
make build                       # Go 六个二进制 → bin/
make serve                       # 起控制面，自动打印访问地址
# 浏览器打开 http://127.0.0.1:7788 —— 空系统会进首启向导：建管理员 → 填模型 key
```

**前置**：Go ≥ 1.27、Node ≥ 20（改前端才需要）、macOS 或 Linux。
不配 key 也能跑：agent 走 shell 模式，能执行命令、能审批，只是不会「思考」。

| 要做的事 | 命令 |
|---|---|
| 跑测试（提交前必跑） | `make test` + `cd web && npm test` |
| 一次跑完全部质量门 | `make check` |
| 端到端冒烟（真起服务跑一轮） | `bash scripts/e2e.sh` |
| 打 macOS 安装包 | `make app` → `dist/Astra-0.2.0.dmg` |
| 打四平台服务器包 | `make release` → `dist/release/` |
| 换 agent 为 dsh（真模型） | 见下方 [接真实模型](#接真实模型dsh--deepseek)，需要 `~/.astra/agent-dsh` |

**dsh 不是必须的**：它是个 139MB 的 Node 裁剪版，没入库。没有它就用 shell agent 开发控制面/前端，功能链路完全一样。

动手前请读 [CONTRIBUTING.md](CONTRIBUTING.md)（分支规范、提交格式、三条不可让步的约束、已知的坑）。

## 一句话架构

控制面是 Go 写的四个二进制（`api` / `worker` / `sandboxd` / `gateway`），跑在 k3s 上、由 APISIX 统一入口；
沙箱层用 CubeSandbox（KVM microVM，兼容 e2b 数据面协议）；**Agent 用 DeepSeek Harness，通过 ACP 协议驱动**；
所有事件先落 PostgreSQL 再推送，任何一端随时接入都能拿到完整历史。

```
 Web / Desktop(Wails) / App
          │  HTTPS + SSE
       APISIX ── 路由 · 鉴权 · SSE 不缓冲
          │
   astra-api ── astra-gateway ── astra-worker
          │                              │  ACP over WSS
          │                       ┌──────┴───────┐
          │                       │  CubeSandbox │
          │                       │  ┌─────────┐ │
          │                       │  │ dsh     │ │  ← agent 进程
          │                       │  │acp-proxy│ │
          │                       │  └─────────┘ │
          │                       └──────────────┘
       PostgreSQL / Valkey / SeaweedFS
```

---

## 三条不可让步的约束

这三条是踩过并否掉方案后留下的硬边界，改动前请先读对应 ADR。

1. **一个会话 = 一个沙箱 = 一个 agent 进程 = 一条 ACP 连接。**
   Session 与 Sandbox 不是永久绑定（沙箱重建时 `session` 不变、只换 `sandbox_id`），
   但同一时刻一条 ACP 连接只能由一个 worker 持有。多端不能直连沙箱，只能订阅控制面转发的事件流。
   → [ADR-0002](docs/adr/0002-agent-in-sandbox-with-acp-proxy.md)

2. **事件必须先写 PostgreSQL，再推送。**
   会话状态挂在 WebSocket 上，"多端对等"就是空话——刷新页面就丢。
   用 `(run_id, seq)` 主键 + `after_seq` 续传；seq 用 `UPDATE runs SET next_seq=next_seq+1 RETURNING`。

3. **LLM key 不进沙箱。**
   沙箱里 agent 的 `base_url` 指向自建网关，只拿短期租户令牌（可撤销、可限额）。
   出网白名单默认拒绝，全量审计 `(run_id, host, bytes)`。

---

## 关键决策速览

| # | 决策 | 选择 | 一句话理由 |
|---|---|---|---|
| 1 | Agent 编排协议 | **ACP v1**，不是 PTY + 正则 | 用协议级阻塞替代解析 ANSI 猜 prompt |
| 2 | Agent 跑在哪 | **沙箱内 + acp-proxy** | worker 重启不丢 agent；沙箱 pause 就是免费 checkpoint |
| 3 | Agent 选型 | **DeepSeek Harness**（`dsh --profile acp`） | MIT 可自编译，第一方 ACP，中文模型成本低一个量级 |
| 4 | 镜像体积 | **826MB → 138MB**（压缩层 25MB） | 46 份 zod 副本用 npm `overrides` 一行去掉 315MB |
| 5 | 编排与入口 | **k3s + APISIX** | 云托管 k8s 违反「源码可控自部署」红线 |
| 6 | 后台管理 | **Vben Admin + 自建 `/admin`**，不用脚手架 | 引入 gin-vue-admin 会出两套 RBAC、两套审计 |
| 7 | 第三方代码 | **源码引入 + 可同步**，不是 `go get` / 镜像引用 | 上游可查、可改、可回滚，不被包管理器锁死 |
| 8 | 沙箱后端 | **provider 抽象**，本地 local / 本地隔离 applecontainer / 生产 cube | 否则 M1 的 worker 在 Mac 上没法开发 |
| 9 | 权限审批 | **阻塞在 ACP `request_permission` 上等人**，超时即拒 | 占位返回等于放行，审批会退化成通知 |
| 10 | 前端交付 | **React + 手写 CSS，产物入库 `go:embed`** | 一个二进制就是完整产品，克隆下来不用装 node |

完整论证在 `docs/`，可复现验证在 `dsh-trim/`，每项决策的取舍记录在 `docs/adr/`。

---

## 目录

```
internal/acp/                # ACP v1 客户端（协议层，无业务依赖）
├── types.go                 # 协议类型：initialize / session / prompt / permission
├── conn.go                  # NDJSON 连接层：通知同步分发保序、请求异步分发
├── client.go                # 高层封装：OnUpdate / OnPermission（默认拒绝）
├── transport.go             # StdioConn：管 agent 子进程，stderr 必须排空
├── wstransport.go           # WSConn：把 WS 消息流适配成 io.ReadWriteCloser
└── conn_test.go             # 7 个用例：保序、超时、错误传播、审批、id 归一化

cmd/
├── acp-proxy/               # 沙箱内运行：把 agent stdio 桥成 WSS，单客户端互斥
├── acp-smoke/               # M0 验证程序：stdio / WSS / 跨进程 resume 三种模式
├── acp-mock-agent/          # 测试用 agent：输出完全确定，单测不依赖模型
├── acp-shell-agent/         # 开发用 agent：prompt 当 shell 命令真执行，无需凭据
├── astra-sandbox/           # 本地沙箱 CLI：up / ls / endpoint / exec / snapshot / rm
└── astra-server/            # M1 控制面：HTTP + SSE + runner（单进程）

internal/runner/             # 一个 run 绑定一个会话：ACP 连接、事件映射、权限决策、审批注册表
internal/store/              # 事件存储接口 + JSONL 本地实现（按 seq 追加、可回放）
internal/eventbus/           # 内存扇出：负责「实时推」，存储负责「不丢」
internal/api/                # REST + SSE + 静态资源
├── api.go                   # 路由、SSE（先订阅再回放）、错误映射
├── files.go                 # 工作区浏览与读取（路径钉死在 workspace 内）
├── static.go                # go:embed 前端产物 + SPA 兜底
└── web/dist/                # 前端构建产物（入库存疑见 ADR-0011，这里选择入库）
internal/sandbox/            # 沙箱后端抽象（一个接口，三个实现）
├── types.go                 # Provider / Sandbox 接口，错误与状态约定
├── local.go                 # 宿主机进程 + sandbox-exec 写限制 + APFS 快照
├── applecontainer.go        # Apple container（macOS 26，每容器一个轻量 VM）
├── cube.go                  # CubeSandbox 控制面（⚠️ 尚未在真实环境验证）
└── helpers.go               # 端口探测 / health 轮询 / token

web/                         # 前端源码（React + TS + Vite，无 UI 框架）
├── src/App.tsx              # 三栏骨架、会话与事件流编排
├── src/api.ts               # REST 客户端 + SSE 订阅（含 overflow 续传）
├── src/timeline.ts          # 事件 → 视觉块（消息/思考/工具/审批）的折叠规则
├── src/components/          # Sidebar / Turn / ToolCard / ApprovalCard / FilePanel / Markdown
└── src/styles.css           # 手写样式，跟着信息密度走

third_party/                 # 上游源码（见「源码引入与上游同步」）
├── upstream.lock.toml       # 唯一事实来源：仓库、commit、引入模式、许可
├── cubesandbox/             # submodule，KVM microVM 沙箱
├── deepseek-harness/        # sparse，ACP 协议实现 + agent 引擎源码
└── envd-proto/              # sparse，只取 5 个 .proto（沙箱内执行服务协议）

scripts/sync-upstream.sh     # check / fetch / update / verify 四动作

docs/
├── 架构设计方案.md           # 主文档：分层、数据模型、ACP 运行时、分期路线、风险
├── 部署与流水线方案.md        # k8s / APISIX / CubeSandbox / CI / GitOps
├── adr/                     # 决策记录（为什么这么定，以及被否掉的是什么）
└── history/
    └── 架构设计方案-v1-已被否决.md   # 旧版，保留作对比，不要照它实施

deploy/
├── k8s/base/                # 四个 Go 服务的 Deployment / Service / HPA / PDB
├── k8s/apisix/              # Gateway API + ApisixRoute（SSE 专用）+ values 覆盖点
├── k8s/cubesandbox/         # Helm values 语义覆盖（KVM / XFS / eBPF 等踩坑项）
├── compose/m0.yaml          # M0 单机环境（不带 k8s，可直接 docker compose up）
└── ci/                      # 可移植构建脚本（不绑 CI DSL）、GitOps bump、smoke

images/agent/Dockerfile      # 三阶段：编 acp-proxy → 裁剪 dsh → 运行时

dsh-trim/
├── Dockerfile               # 裁剪后的 dsh 镜像
├── package.json             # 含 zod overrides —— 体积收益最大的一行
├── trim.sh                  # 七项功能裁剪，每步打印体积
├── trim-patch.yml           # 运行时插件禁用（关遥测、禁用多厂商 SDK）
├── verify-acp.mjs           # 端到端验证：initialize → session/new → prompt
└── README.md                # 裁剪过程与实测数据
```

---

## 源码引入与上游同步

第三方组件不走包管理器、不引用预构建镜像，而是**把源码放进 `third_party/`**。
理由：上游要能查、能改、能回滚；出问题时要能定位到具体一行，而不是对着一个 tag 猜。

`third_party/upstream.lock.toml` 是唯一事实来源，脚本里没有任何硬编码的仓库地址或版本。

| 上游 | 模式 | 为什么是这种模式 |
|---|---|---|
| `cubesandbox` | **submodule** | 79MB，需要 git 历史做 diff；构建时要用它的 Dockerfile |
| `deepseek-harness` | **sparse** | 整仓 317MB 拉不动；审计/扩展只要 `packages/acp/` + `packages/core/` 两棵子树（3.6MB） |
| `envd-proto` | **sparse** | 整仓 99MB 但只要 5 个 `.proto`（29KB），按文件走 contents API |

```bash
make sync-check            # 上游有没有新提交（只读，不动任何文件）
make sync                  # 按 lock 里的 commit 重放全部源码
make sync-verify           # 本地内容与 lock 是否一致
bash scripts/sync-upstream.sh update deepseek-harness   # 升到最新并改写 lock
```

> dsh 走 sparse 要打约 300 次 GitHub API，**匿名限额（60 次/小时）不够**，
> 必须先 `export ASTRA_GITHUB_TOKEN=...`（只需 `public_repo` 读权限）。并发数默认 8，`ASTRA_FETCH_JOBS` 可调。

三个设计取舍：

- **`sparse` 的 `paths` 支持目录**（结尾带 `/` 自动递归展开整棵子树）。这是必需能力而非便利：
  `packages/core/` 有 259 个文件，在 lock 里逐个枚举没法维护——上游加一个文件就漏。
- **`update` 自动改 lock 但不会自动提交。** 改完必须人工 review `git diff third_party/`，
  尤其 `deepseek-harness`（ACP 协议面）和 `cubesandbox`（网络/存储行为）。
- **许可逐项核过。** 只认 OSI 开源（Apache-2.0 / MIT / BSD）。CubeSandbox 的 GitHub 标 `NOASSERTION`，
  查证后确认主体 Apache-2.0，第三方清单中 `GPL-2.0` 那项来自 cilium/bpf 的双许可分支、腾讯选用 BSD-2-Clause。
  唯一的例外是 `hypervisor/docs/` 下的文档为 CC-BY-4.0（署名即合规的引用型许可，不传染），
  详见 [ADR-0007](docs/adr/0007-source-vendoring.md) 与 [NOTICE](NOTICE)。

当前的落地状态（`make sync-verify` 全绿）：

```
cubesandbox        一致 0fea4f8c365f     79 MB   submodule
deepseek-harness   一致 f3803c96974a    3.6 MB   sparse，299 个文件
envd-proto         一致 518278060a15     30 KB   sparse，5 个 .proto
```

---

## M0 验证（已通过）

不需要 KVM，不需要 API key，本地有 Node ≥ 22.15 即可。

```bash
make test        # 7 个协议层单测
make m0-session  # 握手 + 建会话
make m0          # 全链路：initialize → session/new → prompt → permission

make proxy       # 另开一个终端：起 acp-proxy
make m0-ws       # 通过 WSS 再跑一遍
SESSION=<id> make m0-ws-resume   # 杀掉 agent 重启后，用旧 sessionId 恢复
```

实测结论：

| 项 | 结果 |
|---|---|
| `initialize` 握手 | ✅ `image=false / audio=false / embeddedContext=false`，MCP 仅 http（无 sse） |
| `session/new` | ✅ 返回 sessionId，触发 schema 校验与落盘 |
| `session/prompt` | ⚠️ 请求到达 agent，返回 `-32603: llm-deepseek: no API key`（预期，未配 key） |
| `request_permission` 拦截 | ✅ agent 反向请求被客户端接住，未设置处理器时默认拒绝 |
| WSS 经 acp-proxy | ✅ 握手/建会话通过；无 token 返回 401；`/health` 正常 |
| 跨进程 `session/resume` | ✅ 杀掉 agent 后重启，旧 sessionId 恢复成功（靠 `/var/lib/dsh` 持久化） |
| 通知保序 | ✅ 50 条连续通知不乱序（通知同步分发、请求异步分发） |
| 方法名与上游对齐 | ✅ 引入 dsh 源码后逐个核对：`session/new` `prompt` `cancel` `close` `resume` `update` `request_permission` 全部命中 |

关于 `session/resume` 的语义实测澄清：**worker 崩溃重连不需要它**（agent 还活着，会话在内存，
dsh 返回 `session is already active` 是正确行为）；**只有沙箱重建/agent 重启才需要**。

> `images/agent/Dockerfile` 与 `deploy/compose/m0.yaml` **尚未在本机验证**（本机无 Docker 且是 arm64 无 KVM）。
> 首次构建须在 linux/x64 执行，并按 `dsh-trim/README.md` 复核体积是否仍为「落盘 138MB / 压缩层 25MB」。

---

## 本地开发：在 Mac 上跑沙箱

CubeSandbox 要 x86_64 + `/dev/kvm`，开发机是 Apple Silicon，硬搬没戏。
但 M1 的 worker 又必须持有沙箱句柄才能开发——所以沙箱做成了可替换的 provider：

| provider | 隔离 | 用途 |
|---|---|---|
| `local` | 文件系统写限制（Seatbelt），无内核隔离 | 开发主线，零安装 |
| `applecontainer` | 每容器独立内核（轻量 VM） | 本地真隔离验证 |
| `cube` | KVM microVM | 生产 |

```bash
make doctor          # 先看缺什么
make dev             # 起本地沙箱（幂等，可反复跑）
make dev-smoke       # 连上去跑 ACP 握手 + session/new
make dev-cycle       # 一键验证「暂停 → 重启 → 旧会话恢复」
make dev-rm
```

`make dev` 会打印 endpoint 与 token，也可以直接问：

```bash
bin/astra-sandbox endpoint dev url      # ws://127.0.0.1:41146/acp
bin/astra-sandbox endpoint dev token
bin/astra-sandbox exec dev -- ls -la
bin/astra-sandbox snapshot dev base     # APFS clonefile，毫秒级
```

**`local` 的沙箱不是纸面约定**，实测：

- agent 进程写 `$HOME/ASTRA_ESCAPED.txt` → `Operation not permitted`（内核级 Seatbelt 约束）
- 写 workspace 内的相对路径 → 成功
- `pause` → `up` → 端口不变、旧 sessionId 能 `resume` 回来

⚠️ `local` **没有内核隔离、没有网络策略、没有资源配额**，不能跑不可信代码。
客户自传算法必须走 `cube`。详见 [ADR-0008](docs/adr/0008-sandbox-provider-abstraction.md)。

> `applecontainer` 需要先装 [apple/container](https://github.com/apple/container/releases) 的签名 pkg
> 并 `container system start`（要管理员密码），所以它不是默认值。
> `cube` provider 尚未在真实 CubeSandbox 上验证，首次启用前先跑 `doctor` 核对 API 形状。

---

## 本机跑起来（一条命令）

```bash
make serve          # → http://127.0.0.1:7788
```

打开就是工作台：左边建会话，中间下指令，右边看产物。默认 `provider=local`（宿主机进程 + Seatbelt 写限制）、
`agent=shell`（把 prompt 当命令真执行）、`policy=ask`（逐条审批）。

不需要模型凭据，也不需要 Docker / KVM / Node——前端产物已提交并嵌进二进制。

```bash
make web            # 改了前端才需要（node ≥ 20）；产物写到 internal/api/web/dist
make serve POLICY=workspace-write   # 不想被审批打断
make web-dev        # 前端热更新，另开终端跑 make serve

export DEEPSEEK_API_KEY=sk-...      # 接真实模型
make serve-dsh                      # 独立数据目录 ~/.astra/data-dsh
```

## 接真实模型（dsh + DeepSeek）

```bash
export DEEPSEEK_API_KEY=sk-...
make serve-dsh        # → http://127.0.0.1:7788
```

dsh 装在哪由 `sandbox.DefaultAgentCommand()` 决定（`ASTRA_AGENT` → `PATH` 里的 `dsh`
→ `~/.astra/agent-dsh` → 仓库 `dsh-trim`）。key 从启动环境转发进沙箱，不写进任何文件；
要传别的变量用 `-agent-env KEY=VALUE`（可重复）。

实测（本机 `provider=local`、`deepseek-flash`）：

| 场景 | 结果 |
|---|---|
| 一句话问答 | `1+1 等于 2`，0.9s 出结果 |
| 写脚本 + 跑脚本 | `write` → `bash` 两次工具调用，3.6s，事件流里能看到 `python3` 的真实输出 |
| 写工作区外的文件 | Seatbelt 拒绝 → 模型带 justification 申请扩权 → **控制面挂起等人** → 点允许后写入成功 |
| 30 行输出 | `content[]` 里的 stdout 完整落库，刷新页面原样重建 |

接真实模型踩到两组坑，都已经在代码和配置里解决了：

**1. 日志串台会打断协议。** dsh 裁剪安装里 `llm-pi-ai` / `otel` / `session-telemetry-otel`
缺依赖，import 失败时警告写的是 **stdout**——而 ACP 的 NDJSON 也走 stdout。
一行 `llm-pi-ai (...): failed to import` 就让 `session/new` 报
`invalid character 'l' looking for beginning of value`，看着像协议不兼容。
解法：[`internal/sandbox/dsh/astra-trim.yml`](internal/sandbox/dsh/astra-trim.yml)
用 `--patch` 叠加禁用（内嵌进二进制，按内容哈希落到临时目录）。

**2. 沙箱不能嵌套。** 平台用 `sandbox-exec` 包裹 agent，dsh 自己也用 Seatbelt 包裹 bash，
内层 `sandbox_apply` 直接 `Operation not permitted`，agent 的每条命令都失败。
解法：`AgentSpec.SelfSandbox` 显式声明「谁负责隔离」，两层只留一层——
见 [ADR-0012](docs/adr/0012-sandbox-ownership.md)。

> **审批粒度随 agent 变化，界面上照实显示，不美化：**
> `acp-shell-agent` 是 `per-tool`（每次工具调用都问），dsh 是 `escalation`
> （沙箱内自由执行，**申请扩权时才问人**）。顶栏那个 chip 写的就是实际值。
> 想把 dsh 也做成逐条问，需要写一个 dsh 工具策略插件——属于 M3。

### 工作台能做什么

| 能力 | 说明 |
|---|---|
| 会话管理 | 新建 / 重命名 / 删除（删除会连沙箱一起销毁）。标题取首条指令自动生成 |
| 流式对话 | 事件流折叠成消息 / 思考 / 工具卡片 / 审批四类块，刷新页面原样重建 |
| 工具调用 | 按 `toolCallId` 归并 start/update，失败的调用默认展开 |
| 人工审批 | agent 真的阻塞在 `session/request_permission` 上；点「允许」才继续 |
| 停止 | 进程组 kill + 结掉待审批；「取消先于 prompt」的竞态单独处理 |
| 工作区文件 | 浏览沙箱 workspace、预览文本（限制在 workspace 内，软链逃逸会被挡） |
| 断线续传 | SSE 带 `after_seq`；缓冲满时服务端发 `overflow` 主动断开，客户端续传 |
| 重启恢复 | 未结束的 run 归档为失败（不假装续跑），旧会话可继续下指令 |

### 接口

```
POST   /v1/sessions                      建会话（= 起沙箱 + ACP 握手 + session/new）
GET    /v1/sessions                      列会话
PATCH  /v1/sessions/{id}                 改标题
DELETE /v1/sessions/{id}                 删会话（连同沙箱）
GET    /v1/sessions/{id}/files?path=     列工作区目录
GET    /v1/sessions/{id}/file?path=      读文件（≤512KB，二进制只回元信息）
POST   /v1/sessions/{id}/runs            发 prompt，返回 run
GET    /v1/sessions/{id}/runs            列 run
GET    /v1/runs/{id}/history?after_seq=  事件历史（JSON，首屏用）
GET    /v1/runs/{id}/events?after_seq=   SSE：先回放历史再推实时
GET    /v1/runs/{id}/approvals           正在等人的审批
POST   /v1/runs/{id}/approvals/{tcId}    {decision: allow|deny}
POST   /v1/runs/{id}/cancel              取消
GET    /v1/meta                          provider / agent / policy（给前端显示真实值）
```

**SSE 有三条硬语义**（都是踩过才写死的，见 [ADR-0009](docs/adr/0009-control-plane-shape.md)）：
先订阅再回放（否则期间的实时事件会丢）、终态即关连接（否则浏览器会无限重连）、
缓冲满主动断开让客户端带 `after_seq` 续传（丢通知不丢数据）。

**审批也有四条**（见 [ADR-0010](docs/adr/0010-interactive-approvals.md)）：
阻塞在 ACP 请求上而不是排队、请求与结论分两条事件、超时按拒绝且单独记 `timeout`、
取消时先结审批再发 cancel。

### 实测（本机 `provider=local` + `policy=ask`）

| 验证 | 结果 |
|---|---|
| 事件序列 | `run_state` → `tool_call_start` → `approval_req(pending)` → `run_state(waiting_approval)` → `approval_result(allow)` → `tool_call_update(exitCode)` → `run_state(completed)` |
| 审批拦截 | agent 真的停住；点「拒绝」后它走 `refusal` 分支，工具调用标 failed |
| 审批超时 | 无人处理时 `decision=timeout`，按拒绝执行 |
| 持续等待 | 审批等 2 分钟以上不会被命令超时打断（超时只包住命令执行，不包住等人的时间）|
| 取消 | 进程组被 kill，终态 `cancelled`；并发/提前取消都不会丢 |
| 重启控制面 | 中断的 run 归档为失败；重连时先 cancel 再 resume，旧会话照常可用 |
| `after_seq` 续传 | 只返回该序号之后的事件 |
| 文件越界 | `../meta.json`、指向 `/etc` 的软链都返回 403 |
| 浏览器点审批 | 无头 Chrome 点「允许」→ run 从 `waiting_approval` 走到 `completed` |

⚠️ 没有鉴权，只绑 `127.0.0.1`。单副本，审批注册表在内存里（多副本要迁 PG）。


---

## 平台层（M1.7）

设计见 [`docs/平台化设计-认证与治理.md`](docs/平台化设计-认证与治理.md)，实现在 `internal/{auth,store/sqlstore,obs,llm}`。

### 跑起来

```bash
make serve              # 默认 sqlite，首次启动自动播种租户并打印管理员密码（只打印一次）
```

首次启动会看到：

```
   租户 slug : default
   管理员    : admin@astra.local
   密码      : <随机 16 位>
```

打开 <http://127.0.0.1:7788> 用这组凭据登录。顶栏「管理后台」有用户 / API Key / 用量 / 审计 / 沙箱 / 回收 / 租户七个页签。

![登录页](docs/images/login-real.png)

![用户管理](docs/images/admin-users-real.png)

![用量](docs/images/admin-usage-real.png)

**不想被登录挡住的场合**（本地调试、做 demo）：

```bash
make serve ARGS="-require-auth=false"   # 带凭据就解析，不带也放行
./bin/astra-server -store file ...      # 退回 M1 的单文件存储，无租户/无计费
```

### 存储

| `-store` | 说明 |
|---|---|
| `sqlite`（默认） | 纯 Go 驱动，无 cgo、无外部服务。单机可用，多副本不可用 |
| `postgres` | 生产用。9 张表 + 11 索引，与 sqlite 共用一套语句（靠 `rebind` 适配占位符） |
| `file` | M1 旧实现。不支持租户、用量、审计，仅作降级路径 |

DSN：`-dsn postgres://user:pass@host:5432/astra`，或留空用 `<data>/astra.db`。

### 权限模型

三个角色，权限是白名单（新增动作忘了授权时默认拒绝）：

| 角色 | 能做什么 |
|---|---|
| `owner` | 全部，含改租户额度、停用租户 |
| `admin` | 管用户、管凭据、管沙箱、看全租户用量与审计；不能动租户本身 |
| `member` | 只能用自己的会话，只看得到自己的用量 |

凭据两种：登录拿 JWT（24h），或签发 API Key（`astra_sk_` 开头，明文只显示一次）。

```bash
curl -H "X-API-Key: astra_sk_..." http://127.0.0.1:7788/v1/sessions
```

### 可观测

```bash
curl http://127.0.0.1:7788/metrics | grep ^astra_
curl http://127.0.0.1:7788/readyz   # 真实探测 store 与 admin，不只是回 ok
```

指标含 `astra_runs_total{status}`、`astra_run_duration_seconds`、`astra_approvals_total{decision}`、`astra_http_requests_total{method,path,status}`、`astra_llm_tokens_total`、`astra_llm_cost_micro_usd_total` 等。审计事件异步入库，不进主流程的关键路径。

### LLM 网关与计费

**真 key 已经不进沙箱了。** 每个会话创建时签发一张 2 小时有效期的虚拟 key，
以 `DEEPSEEK_API_KEY` + `DEEPSEEK_BASE_URL` 注入 agent 进程；agent 打网关，
网关换成真 key 打上游并记账。

这一条能成立的关键事实：**dsh 走的是 Anthropic Messages 协议**（`{baseURL}/v1/messages`），
不是 OpenAI 的 `/v1/chat/completions`。所以网关按协议族分流，Messages 的 usage 从
`message_start`（input）与 `message_delta`（output，累计值不是增量）合并而来，
且**不给 Messages 请求注入 `stream_options`**——那是 OpenAI 专有字段。

```bash
make serve-dsh ARGS="-llm-gateway -llm-key sk-... -llm-public-url http://127.0.0.1:7788"
```

沙箱里 `ps eww` 看到的是：

```
DEEPSEEK_API_KEY=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...   # JWT，2h 过期
DEEPSEEK_BASE_URL=http://127.0.0.1:7788/v1/llm/anthropic
```

实测（本机，真 dsh + 真 DeepSeek）：

| 观测点 | 值 |
|---|---|
| run 完成耗时 | 4s，回复「在线」 |
| `usage` 表 | `deepseek-v4-flash` in=341 out=15, `source=gateway` |
| `/metrics` | `astra_llm_tokens_total{direction="input"} 599` / `{output} 350` |
| 流式透传 | 33 个 `content_block_delta` 逐帧到达，不是攒完再发 |

**价格表仍是占位常量**（`internal/llm/pricing.go` 标了「需按最新价目表核对」）。
在接真实价目表之前，`costMicroUsd` 只能当「有调用发生」的信号，不能当账单。

### 沙箱生命周期

沙箱不会自己消失：客户端关标签页、进程崩过、用户忘了删，都会留下没人用的资源。
`internal/lifecycle` 用后台扫描兜底：

| 判据 | 默认 |
|---|---|
| 孤儿（provider 里有、会话表里没有） | 一律回收 |
| 会话已非 active | 一律回收 |
| 存活超过 `-sandbox-max-age` | 6h |
| 无活动超过 `-sandbox-idle` | 30m |
| 每租户并发沙箱上限 `-sandbox-quota` | 0（不限） |

**回收只销毁沙箱，不删会话数据**——历史消息、工具调用、审批记录都还在，
下次发指令会重新起一个沙箱。这与「强杀沙箱」的语义一致。

配额是**准入控制**：超限时建会话直接 429，而不是先创建再被回收。

```bash
curl -s localhost:7788/v1/admin/lifecycle        # 策略 + 上一轮报告
curl -X POST localhost:7788/v1/admin/lifecycle/sweep   # 立即清理
```

管理后台的「回收」页把这几个数字可视化，截图：[`docs/images/admin-lifecycle-real.png`](docs/images/admin-lifecycle-real.png)。

### 契约

- `GET /v1/openapi.json` — 运行时导出
- `GET /docs` — 自包含浏览页（不依赖任何 CDN）
- 契约正文在 `internal/api/openapi.yaml`（放在这里是因为 `//go:embed` 不能引用父目录；`docs/openapi.yaml` 是跳转说明）
- `internal/api/openapi_test.go` 做双向防漂移：契约缺路由会红，代码多路由也会红；schema 字段名直接与 Go struct 的 json tag 比对

---

## 路线

| 阶段 | 目标 | 关键验证 | 状态 |
|---|---|---|---|
| **M0** | 协议层可行性 | ACP 全链路、`request_permission` 拦截、`session/resume` 恢复 | ✅ 完成 |
| **M1** | 可演示版本 | Web 单端 + 事件落库 + SSE；单租户 | ✅ 本机跑通（控制面 + 事件流 + 取消 + 重启恢复） |
| **M1.5** | 能用的工作台 | 会话/文件/审批/停止全在界面上完成 | ✅ 完成（见上方截图）；K8s 化与 PG 存储仍未做 |
| **M1.6** | 接真实模型 | 真实 agent 在沙箱里写代码、跑命令、申请扩权并等人审批 | ✅ dsh + DeepSeek 本机验证；沙箱归属见 ADR-0012 |
| **M1.7** | 平台化 | 登录与权限、租户隔离、SQL 存储、沙箱管理面、指标与审计、LLM 网关与计费、OpenAPI | ✅ 六块可用（见下方「平台层」） |
| **M1.8** | 网关闭环与生命周期 | 真 key 不再进沙箱、Messages 协议记账、沙箱回收与配额、前端测试、部署可执行化 | ✅ 详见 [`docs/平台化设计-二期.md`](docs/平台化设计-二期.md) |
| M2 | 桌面端 | Wails v2，本地目录挂进沙箱、离线事件缓存 | 未开始 |
| M3 | 移动端 + 审批流转 | App 查看/批准 | 部分（单机审批已通，多人流转未做） |
| M4 | 多租户 | 租户隔离、配额、审计、agent 可替换 | 部分（隔离 + 配额准入 + 沙箱回收已做；沙箱池化未做） |

**M0 不跑 k8s。** CubeSandbox on k8s 的节点预检（KVM / XFS reflink / bpffs / CIDR 冲突）+ node-init 至少 3 天，
而这 3 天不产生任何协议层结论。`deploy/` 里清单已备好，M1 切过去只是换一次 sandbox provider 配置。

M0 之后已补上 `internal/sandbox` 抽象（本地进程 / Apple container / CubeSandbox 同一接口），
所以 M1 写 worker 时可以直接在 Mac 上跑完整链路，换生产 provider 只是改一个参数。

---

## 许可证

本项目以 [Apache-2.0](LICENSE) 授权，Copyright 2026 Wang Yulong。

第三方组件的完整署名与免责声明见 [NOTICE](NOTICE)。

### 上游依赖

源码引入的依赖：

| 组件 | 许可 |
|---|---|
| [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) | Apache-2.0（内嵌 kata-containers / cloud-hypervisor 为 Apache-2.0，cilium/bpf 取 BSD-2-Clause） |
| [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | MIT |
| [envd-proto](https://github.com/e2b-dev/runtime) | Apache-2.0 |

版本、commit、引入理由与许可备注的唯一事实来源是 [third_party/upstream.lock.toml](third_party/upstream.lock.toml)，
逐项分析见 [docs/adr/0007-source-vendoring.md](docs/adr/0007-source-vendoring.md)。
Go 模块依赖以 Apache-2.0 / MIT / BSD 为主，其中 `github.com/prometheus/*` 各带 NOTICE，已收录进 [NOTICE](NOTICE)。

### 许可红线

**只收 Apache-2.0 / MIT / BSD。** 每条依赖都逐项查过原始 LICENSE，不看 GitHub 的自动标签
（CubeSandbox 被标 `NOASSERTION`，实测只是「混合许可 + 腾讯自定义前言」，主体是 Apache-2.0；
其清单里的 `GPL-2.0` 来自 cilium/bpf 的双许可分支，腾讯已选择 BSD-2-Clause）。

source-available 与 copyleft 一律不进内核组件。**已评估并否掉**的清单（理由随附）：
EMQX ≥5.9、Redis ≥7.4、MinIO、Grafana、Loki——见 `docs/` 下对应的选型章节。

例外只有一处：CubeSandbox 的 `hypervisor/docs/` 文档是 CC-BY-4.0，属署名即合规的引用型许可，
不具传染性，随源码原样保留并在 [NOTICE](NOTICE) 中署名。

安全问题请走 [SECURITY.md](SECURITY.md) 的私密渠道，不要开 public issue。
