# 0013 · deepseek-harness 改为 submodule + 稀疏检出

> 状态：已采纳（2026-11-05）· 部分取代 [ADR-0007](0007-source-vendoring.md) 里 deepseek-harness 的引入方式

## 背景

ADR-0007 把 deepseek-harness 以 `sparse` 模式引入：用 GitHub contents API 逐文件下载
`packages/acp/` + `packages/core/` 两棵子树，落成 299 个普通文件提交进仓库。

三个问题：

1. **GitHub 上点不进去**。cubesandbox 是 submodule，网页上显示为指向原仓库的链接；
   deepseek-harness 是一堆复制文件，看不出上游在哪、对应哪个 commit。审计时只能「相信这份拷贝」。
2. **升级成本高**。改版本要重新下载 300 个文件并全量提交 diff。
3. **归属与许可靠人记**。MIT 要求保留版权声明，但复制进来的文件脱离了上游目录结构。

当时没直接上 submodule 的真实原因是：整仓 351.8MB，`git clone` 在本机持续
`Operation too slow (<1000 bytes/sec)`，抓不下来。ADR-0007 的结论受限于当时的网络条件，不是方案本身不好。

## 决定

改成 submodule，用 **blobless clone + 浅克隆 + sparse-checkout** 解决体积问题：

```bash
git clone --filter=blob:none --no-checkout --depth 1 --single-branch <url> third_party/deepseek-harness
git sparse-checkout init --cone && git sparse-checkout set packages/acp packages/core
```

实测 **27 秒**拉完（351.8MB 的仓库，只下载 commit + tree 对象，不下载 blob），
工作区落地 345 个文件 / 2.7MB。对比：不加 `--filter=blob:none` 的浅克隆跑了 5 分钟仍在握手阶段。

关键取舍：**裁切用 sparse-checkout，不用删文件。**

| | 删文件（原方案） | sparse-checkout（现方案） |
|---|---|---|
| submodule 状态 | 变脏，`git status` 有改动 | 干净，等于上游原样 |
| GitHub 上看到的 | 一份被裁剪的副本 | 干净上游 + 具体 commit |
| 升级 | 重新下载并提交 300 文件 diff | 改一行 commit |
| 工作区体积 | 2.7MB | 2.7MB（连 `.git` 共 41MB） |

`.gitmodules` 里 `url` 写 **https**（陌生人没有 SSH key 也能拉），`shallow = true`；
本机 https 被代理拦着，实际拉取时用 `-c url."git@github.com:".insteadOf=https://github.com/` 走 22 端口。

## 影响

- `bash scripts/sync-upstream.sh fetch` 现在是唯一入口：submodule 已就绪则跳过网络，
  未就绪则拉取并做稀疏检出。`paths` 字段对 submodule 模式同样生效。
- **不拉也不影响构建**：构建用 npm 装的 dsh 裁剪版（`dsh-trim/`），这份源码只用于审计/扩展/对比。
  所以 CI 不递归 submodule，clone 下来的空目录不报错。
- 顺带修了 `sync-upstream.sh` 的代理探测：`git config http.proxy` 里留着 Clash 的 7897，
  Clash 关掉后每个请求都要先撞一次连接失败，表现为「下载挂了」。现在先探一次，不通就走直连。

## 代价

- 首次拉取仍需网络且约 27 秒；离线机器上 `third_party/deepseek-harness` 是空目录（可接受）。
- `.git` 从 0 变成 32MB（blobless 的 tree + commit 对象）。对 clone 主仓库有轻微影响，
  但比把 351MB 历史拖进来好得多。
