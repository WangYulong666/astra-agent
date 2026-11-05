# 0007. 上游依赖一律源码引入，且必须可同步

- **状态**：已采纳
- **日期**：2026-10-28
- **相关**：`third_party/README.md`、`scripts/sync-upstream.sh`

## 背景

项目要求：**所有第三方组件都要源码引入，并且后续能继续从上游仓库更新代码。**

这与既有红线一致（全套源码可控自部署），但直接违反直觉的做法有两个：

- `docker pull` 官方镜像 → 改不了。CubeSandbox 的 CubeEgress 默认拒绝网段会覆盖 k8s pod/service CIDR，必须 patch
- `npm install` 直接用 → 看不到内部实现，审计不了遥测行为，也写不了插件

## 决策

### 1. 单一事实来源：`third_party/upstream.lock.toml`

所有上游的 URL、commit、许可、引入理由都在这一个文件里。**脚本里不允许出现硬编码的仓库地址或版本**——升级只改表，不动脚本。

commit 必须是完整 40 位 sha，不接受 branch 或 tag（branch 会漂移，tag 可以被移动）。

### 2. 三种引入模式，按「需要多少」选

| 模式 | 适用 | 落地方式 | 升级 |
|---|---|---|---|
| `submodule` | 要完整源码、可能改 | gitlink，源码按需拉 | `sync-upstream.sh update <name>` |
| `sparse` | 只要几个文件，或只要某几棵子树 | 按 commit 逐个下载 + 校验；目录项（以 `/` 结尾）自动递归展开 | 同上 |
| `tarball` | 要完整源码，但 git 协议拉不动且链路撑得住大文件 | 快照解压，**源码不入主库**、只提交 `SOURCE` | 同上 |

选 `sparse` 而不是 `submodule` 的两个场合，理由都是体积悬殊：

- envd：整个 runtime 仓库 99MB，实际需要的 5 个 `.proto` 加起来 29KB。
- deepseek-harness：整仓 317MB，而审计/扩展真正需要的 `packages/acp/` + `packages/core/` 只有 2.7MB。

`sparse` 的目录展开是**必须**的能力，不是便利功能：`packages/acp/` 下有 35 个文件、`packages/core/` 下有 259 个，
在 lock 里逐个枚举根本没法维护（上游加一个文件就漏）。展开走 `git/trees?recursive=1`，一次请求拿整棵子树。

### 3. `scripts/sync-upstream.sh` 四个动作

```bash
check              只报告哪些上游有新提交，不改任何文件
fetch [name]       按 lock 里的 commit 重放源码（新 clone / CI 用）
update <name>      升级到最新并改写 lock
verify             校验本地内容与 lock 是否一致
```

`update` 之后**必须人工 review diff**。两个来源尤其要看：

- **deepseek-harness**：ACP 相关改动直接影响协议兼容，上游自己声明会有破坏性变更
- **cubesandbox**：网络 / 存储后端改动会影响 k8s 部署参数（见 `deploy/k8s/cubesandbox/values.yaml`）

### 4. Go 依赖走 `go mod` + 模块代理，不用 `vendor/`

Go 依赖不落在 `third_party/`：版本由 `go.mod` 锁、内容由 `go.sum` 的 `h1:` 摘要锁，
源码由模块代理（`GOPROXY`）在构建时拉进本机 `GOMODCACHE`。升级路径是 `go get <pkg>@<ver>` + `go mod tidy`。

**本仓库没有 `vendor/` 目录**（`.gitignore` 也把 `vendor/` 挡掉了）。原因：vendor 会把每次
依赖升级的完整源码快照写进主仓库历史，而它带来的收益（离线构建）在本项目的构建环境里不需要；
代价却是 `go mod vendor` 忘记刷新就出现「go.mod 说 A、磁盘上是 B」的静默不一致。

因此首次构建需要能访问模块代理，或者 `GOMODCACHE` 里已有这些模块。

## 落地清单

| 依赖 | 模式 | 体积 | 许可 | 为什么这样选 |
|---|---|---|---|---|
| CubeSandbox | `submodule` | 79MB | Apache-2.0 主体；3 个内嵌第三方（Apache/BSD）；`hypervisor/docs/` 为 CC-BY-4.0 | 可能要 patch 网络策略；git 能拉得动 |
| deepseek-harness | `sparse` | 3.6MB / 299 文件 | MIT | 整仓 317MB 拉不动；审计只要 acp + core 两棵子树 |
| envd-proto | `sparse` | 30KB / 5 文件 | Apache-2.0 | 整仓 99MB，只要 5 个 `.proto` |
| APISIX | 不入库 | — | Apache-2.0 | 走 helm chart 部署，不是代码依赖 |
| Vben Admin | 待定 | — | MIT | M1 起单独决定 |

`tarball` 模式仍保留在脚本里（dsh 把 `mode` 改回 `"tarball"` 即可切全量快照），
只是在当前链路下不可行，所以不作为默认。

## 实测数据（2026-10-28，本机经 Clash 代理 127.0.0.1:7897）

| 通道 | 目标 | 结果 |
|---|---|---|
| `git clone` | CubeSandbox（57.8MB） | ✅ 成功 |
| `git clone` | deepseek-harness（317.3MB） | ❌ `RPC failed; curl 28 Operation too slow. Less than 1000 bytes/sec` |
| `codeload` tarball | deepseek-harness 单 commit | ⚠️ 起手 254 KB/s，持续掉到 11–50 KB/s；多次 180s/300s 超时，最高只到 19MB |
| `api.github.com` 单文件 / 小树 | 任意文件 | ✅ 稳定 1.0–1.6s，不随累计流量衰减 |
| `raw.githubusercontent.com` | 任意文件 | ❌ 直连与走代理均返回 000 |

**三条经验**：

1. git 的 fetch-pack 在这个网络下被限速到不可用，codeload 只在小文件上可用，**大文件持续传输会掉速**。
2. `raw.githubusercontent.com` 在本机不可用，一律用 `contents` API + `Accept: application/vnd.github.raw`。
3. 链路对大响应有惩罚、对小请求没有 → **sparse（多小请求）反而比 tarball（单大请求）快得多**：
   299 个文件并发 8 路，62 秒落地；而单个 tarball 跑了 15 分钟只到 19MB。

## 后果

### 好的

- 每个依赖都能回答三个问题：**这个 commit 是哪个、许可是什么、为什么引入**
- 上游一改就能 `check` 出来，升级是显式动作而不是默默漂移
- 完全离线可用（fetch 一次之后不再依赖网络）
- patch 有地方放（`patches/<name>/*.patch`），不会散落在代码里

### 坏的

- **主仓库不会因为 submodule 变小**：CubeSandbox 的 gitlink 很轻，但首次 `fetch` 要拉 79MB
- **clone 之后不能直接编译**：必须跑一次 `sync-upstream.sh fetch`（与 submodule 的 `--recursive` 同样的心智负担）
- **`tarball` 模式拿不到 git 历史**：不能 `git log`/`git diff` 上游，只能靠 `patches/` 目录记录我们的改动
- **dsh 只入了两棵子树**（acp + core，3.6MB），不是全仓。想读 `apps/` / `native/` / `website/` 时得临时切 tarball。
  这是有意识的取舍：全量 317MB 在 CI 每次拉一遍不现实，而 99% 的审计与扩展需求落在这两棵子树里

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| 只用包管理器（docker pull / npm install / helm） | 改不了、审计不了、插件写不了。CubeEgress 的 CIDR 问题就是必须改代码的例子 |
| 全部 `submodule` | dsh 的 git clone 在当前网络下直接失败（实测），方案不可行 |
| 全部 vendor 进主仓库 | 317MB 的 dsh + 79MB 的 CubeSandbox 会让 `git clone` 变成十分钟起步，且每次上游更新都在主仓库历史里留一堆噪音 |
| `git subtree` | 把上游历史混进主仓库（历史污染），且同样受 git 协议限速影响 |
| 只锁版本号，不落源码 | 上游删库 / 改名就找不回来了。这次查 CubeSandbox 许可时就发现 GitHub 把它标成 `NOASSERTION`，不读源码根本判断不了 |
