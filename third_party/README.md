# third_party —— 源码引入的上游依赖

本目录存放**从上游仓库引入的源码**。原则是「源码可得、可改、可审计，且能安全地跟上上游」。

## 为什么不是包管理器直接装

| 方式 | 问题 |
|---|---|
| `docker pull` 官方镜像 | 改不了。CubeSandbox 的 CubeEgress 默认拒绝网段会覆盖 k8s CIDR，必须 patch |
| `npm install` 直接用 | 看不到内部实现，审计不了遥测行为，也无法写插件 |
| 只锁版本号 | 上游仓库删了 / 改名了就找不回来了 |

## 锁定表

所有上游的 URL、commit、许可、引入理由都在 **[`upstream.lock.toml`](upstream.lock.toml)**。

**脚本里不允许硬编码 URL 或版本**，一律读这张表。升级只改表，不动脚本。

## 四种模式

| mode | 适用 | 落地位置 | 源码入库 | 升级方式 |
|---|---|---|---|---|
| `submodule` | 要完整源码、要 git 历史、可能改 | `third_party/<name>/`（gitlink） | ✅ | `sync-upstream.sh update <name>` |
| `sparse` | 只要几个文件，或只要某几棵子树 | `third_party/<name>/`（普通文件） | ✅ | 同上 |
| `tarball` | 要完整源码，且链路撑得住单次大传输 | `third_party/<name>/`（普通文件） | ❌ 只入 `SOURCE` | 同上 |
| `gomod` | Go 库依赖 | 不进本目录（由模块代理拉取到 `GOMODCACHE`） | ❌ 只入 `go.mod` / `go.sum` | `go get` → `go mod tidy` |

选型不看偏好，看实测：

- **`sparse` 而不是 `submodule`**：envd 整个 runtime 仓库 99MB，实际需要的 5 个 `.proto` 加起来 29KB。
- **`sparse` 而不是 `tarball`**：deepseek-harness 整仓 317MB。本机链路对大响应持续掉速
  （codeload 起手 254KB/s，随后掉到 11–50KB/s，单个 tarball 跑 15 分钟只到 19MB），
  而对小请求稳定 1 秒内返回。改成「按需取子树」后 299 个文件并发 8 路、62 秒落地。
- **`tarball` 仍然保留**：需要全量快照时把 lock 里的 `mode` 改回 `"tarball"` 即可，脚本不用动。
- **`gomod` 不用 `vendor/`**：Go 依赖由 `go.mod` 锁版本、`go.sum` 锁内容摘要，
  源码由模块代理（`GOPROXY`）拉进本机 `GOMODCACHE`。这意味着仓库里**没有** `vendor/` 目录，
  首次构建需要能访问代理（或已缓存）。`vendor/` 会让每次依赖升级都在主仓库历史里留一份
  完整源码快照，且 `go mod vendor` 之后忘记刷新就是一类静默的版本不一致，
  所以这里明确不采用。

`sparse` 的 `paths` 支持两种写法：

```toml
paths = [
  "LICENSE",            # 单个文件，原样下载
  "packages/acp/",      # 目录，结尾带 / 时递归展开该子树下所有文件
]
```

目录展开是必需能力而不是便利：`packages/core/` 有 259 个文件，在 lock 里逐个枚举无法维护
（上游加一个文件就漏）。实现走 `git/trees?recursive=1`，一次请求拿整棵子树。

并发数默认 8，可用 `ASTRA_FETCH_JOBS` 调整。

## 同步脚本

```bash
bash scripts/sync-upstream.sh check            # 只报告哪些上游有新提交，不改任何文件
bash scripts/sync-upstream.sh fetch            # 按 lock 里的 commit 重放（新 clone / CI 用）
bash scripts/sync-upstream.sh update <name>    # 升级到上游最新，并改写 lock
bash scripts/sync-upstream.sh verify           # 校验本地内容与 lock 是否一致
```

> 必须用 `bash` 而不是 `sh`：脚本用了进程替换 `< <(...)` 和 `set -o pipefail`，POSIX sh 跑不了。

`update` 之后**必须人工 review diff 再提交**，尤其两个来源：

- **deepseek-harness**：ACP 相关改动直接影响协议兼容，上游自己声明会有破坏性变更
- **cubesandbox**：网络 / 存储后端改动会影响 k8s 部署参数（见 `deploy/k8s/cubesandbox/values.yaml`）

## 首次 clone

submodule 不在默认 clone 里，需要显式初始化：

```bash
git clone --recurse-submodules https://github.com/WangYulong666/astra-agent.git
# 或者已 clone 之后：
bash scripts/sync-upstream.sh fetch
```

## 网络前提（本机实测，2026-10 记录）

`api.github.com` / `codeload.github.com` 直连可用，`git clone github.com` 和 `raw.githubusercontent.com` 不可用。
所以脚本里：

- **sparse 模式不走 `raw.githubusercontent.com`**，改走 `api.github.com/repos/.../contents/<path>?ref=<sha>`
  + `Accept: application/vnd.github.raw`，拿到的是同样的原始字节。
- **tarball 模式不限总时长**（`--max-time 0`），靠 `--speed-limit 2048 --speed-time 60` 判「卡死」而不是「慢」。
  codeload 是边生成边推流，不返回 `Content-Length`、不支持 `Range`，所以断点续传在这里无意义。
- 匿名 API 限额 60 次/小时，批量拉取时给 `ASTRA_GITHUB_TOKEN`（只需 public_repo 读权限）。
- 需要走代理时设 `ASTRA_PROXY`，未设则沿用仓库的 `git config http.proxy`。

## 添加新的上游依赖

1. 在 `upstream.lock.toml` 里加一个 `[[source]]`，写清 `purpose` 和 `license`
2. 跑 `bash scripts/sync-upstream.sh fetch`
3. 如果带自定义修改，改动写在 `patches/<name>/*.patch`，并在 lock 的 `note` 里说明
4. 许可必须过红线：**只认 Apache-2.0 / MIT / BSD**。source-available（BSL / RSALv2 / SSPL / AGPL）
   一律不进内核组件，需要时单独开 ADR 论证
