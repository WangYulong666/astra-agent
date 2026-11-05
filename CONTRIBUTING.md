# 参与开发

先读 [README](README.md) 的「新成员从这里开始」，五分钟能跑起来再说别的。

## 提交前必做

```bash
make check        # gofmt + vet + go test + 前端测试，一条命令全过
bash scripts/e2e.sh   # 改过控制面/沙箱/runner 时，真起服务跑一轮
```

CI 跑的是同样的东西。**本地必须全绿再推**——不要让 CI 替你发现问题。

## 分支与提交

- 分支：`main` 保护，一律开分支 + PR。命名 `feat/xxx`、`fix/xxx`、`docs/xxx`
- 提交信息用中文，第一行说明「做了什么」而不是「改了哪个文件」：

```
fix(build): 修 .gitignore 误伤源码——credentials.go 一直没入库

*credentials* 这条密钥防护规则把源码一起挡掉了，本机因为文件还在所以编译正常，
但任何人 clone 下来 make build 都报 undefined。
```

- 一次提交一件事。重构和功能改动分开，否则 review 时无法判断哪部分引入了风险

## 三条不可让步的约束

改架构前先读 [ADR](docs/adr/)，这三条是踩过坑否掉方案后留下的：

1. **一会话 = 一沙箱 = 一 agent 进程 = 一条 ACP 连接**（ADR-0002）
2. **事件先落库再推送**，`(run_id, seq)` + `after_seq` 续传（ADR-0003）
3. **真 key 不进沙箱**，agent 只拿短期虚拟 key，经网关记账（ADR-0004）

破坏其中任何一条的 PR 会被打回，不是风格问题。

## 代码约定

- 注释写「为什么」，不写「是什么」。踩过的坑、否掉的方案、反直觉的地方必须写进去
- 新依赖先进 `third_party/upstream.lock.toml`（`make sync` 拉取），不要在 `go.mod` 里随手加
- 前端样式：布局改 `web/src/styles.css`，视觉改 `web/src/theme.css`（覆盖层）。**不要混着改**
- 数据库结构变更必须是**新增**幂等迁移（`internal/store/sqlstore/migrations`），改已有迁移对老库永不生效

## 已知的坑（都真踩过）

| 坑 | 表现 | 正确做法 |
|---|---|---|
| `.gitignore` 的 `*credentials*` | 源码文件没入库，本机正常、别人 clone 编译失败 | 加 `!*.go` 例外；新文件提交后跑一次干净 clone 验证 |
| `web/package-lock.json` 绑镜像 | 本机 npmrc 是淘宝源，lock 固化镜像地址，CI（境外）必红 | CI 里 sed 改回官方源再 `npm ci` |
| Wails 交叉编译 | `go build ./cmd/astra-desktop` 不带 tag 编不出桌面端 | `-tags "desktop,production"`，且 macOS 需要 `CGO_LDFLAGS="-framework UniformTypeIdentifiers"` |
| 命令行模式重启掉登录 | JWT 密钥每次随机生成 | 已修：`internal/app` 统一下沉到 `dataDir/jwt-secret` |
| macOS 沙箱不能嵌套 | 平台 `sandbox-exec` + dsh 自带 Seatbelt = 内层全崩 | `AgentSpec.SelfSandbox` 声明谁负责隔离，只留一层 |
| 长路径 `overflow-wrap: anywhere` | 窄屏断成三行、越读越乱 | `white-space: pre` + `overflow-x: auto` + 右缘渐隐 |
| bash 3.2 | `"$var）"` 报了 unbound | 全角字符紧邻时写 `${var}` |

## 目录速查

```
cmd/            六个入口：server / desktop / sandbox / acp-proxy / acp-shell-agent / smoke
internal/api/   控制面 HTTP：路由、认证、SSE、审批、计费、OpenAPI
internal/runner 一次 run 的生命周期与审批状态机
internal/sandbox local / applecontainer / cube 三种实现 + dsh 适配
internal/store  File / sqlite / postgres 三套存储（同一接口）
web/src/        React 工作台（styles.css 布局 / theme.css 视觉）
app-android/    Flutter WebView 壳（Android）
desktop/        Info.plist、图标、entitlements
scripts/        build-app / build-release / e2e / notarize / sync-upstream
docs/           使用说明、ADR、部署清单、截图
third_party/    上游源码（锁定表驱动，不手改）
```

## 贡献许可（入站 = 出站）

本项目以 [Apache-2.0](LICENSE) 分发。为了避免「贡献进来、发不出去」，入站贡献条款必须与出站一致：

**采用 DCO（Developer Certificate of Origin 1.1）。** 不需要签 CLA，但每次提交必须带签署行：

```bash
git commit -s -m "fix(build): 修 .gitignore 误伤源码"
# 生成的行：Signed-off-by: Your Name <you@example.com>
```

签署 `-s` 即表示你声明：该贡献由你原创，或你有权按 Apache-2.0 提交，并且同意以 Apache-2.0 授权本项目的分发
（DCO 全文见 https://developercertificate.org/）。**提交 PR 即视为对整个 PR 内所有提交做此声明。**

目前 CI 没有单独的 DCO 检查（只有 `make check` 那一套），所以签署状态由 review 时人工确认。
忘了签就 `git rebase --signoff HEAD~N` 补上，不要新开一个「补签署」的提交。

两条附加约定：

- 引入第三方代码（copy 一段实现、贴一个算法）必须在 PR 描述里说明来源与许可，且仍需过
  `third_party/` 的许可红线（只认 Apache-2.0 / MIT / BSD）
- 不要提交从别处抄来但许可不明（无 LICENSE、source-available、GPL/AGPL）的代码

## 提 PR

1. 标题同上（`fix(scope): 中文说明`）
2. 描述里写：**为什么要改 / 怎么验证的 / 有什么已知取舍**
3. 涉及 UI 的贴截图（放 `docs/images/`，沿用现有命名）
4. 涉及数据结构、协议、沙箱边界的，先在 issue 里说清楚再动手
