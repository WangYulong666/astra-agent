# 平台能力扩展 v2：出网 / 调度 / 沉淀 / 连接器 / 浏览器

> 对标 Meta Muse 与 Grok Bot 后补的六块能力。接口与路由已由主控冻结（`internal/*/`, `internal/api/*_admin.go`），
> 各任务只填 **包内实现**，不改冻结文件。

## 一、模块与文件归属

| 模块 | 独占 | 冻结（禁止改） |
|---|---|---|
| A 出网管控 | `internal/egress/**`（除 `egress.go`）、`internal/api/egress_admin.go` | `api.go`、`go.mod`、`app/**`、`sandbox/**`、`runner/**` |
| B 调度 | `internal/schedule/**`（除 `schedule.go`）、`internal/api/schedule_admin.go` | 同上 |
| C 技能沉淀 | `internal/distill/**`（除 `distill.go`）、`internal/api/distill_admin.go` | 同上 |
| D 连接器/凭据库 | `internal/connector/**`（除 `connector.go`）、`internal/api/connector_admin.go` | 同上 |
| E 浏览器 | `internal/browser/**`（除 `browser.go`）、`internal/api/browser_admin.go` | 同上 |

需要新增依赖/路由/装配行时**不要自己改**，在汇报里列出「需要主控加的几行」。

## 二、已完成的地基（不要重复造）

- `internal/app/capabilities.go` 已按环境变量装配好七个能力，每个都遵循
  **「配了就开，起不来只记日志」**：`Open` 返回 error 时能力降级为 nil，平台照常启动。
- `/v1/meta` 已输出 `egress / scheduler / distill / connectors / browser` 开关。
- 权限已挂好：改出网策略要 `tenant:manage`，看日志要 `audit:read`，
  管连接器要 `key:manage`，其余走 `session:read` / `run:create`。
- **沙箱环境注入由主控统一做**：各包只导出
  `func EnvFor(...) (map[string]string, error)` 或说明需要哪些变量，不要改 runner。

## 三、各模块的实现要点

### A 出网（`egress.Open`）
- HTTP CONNECT 代理 + 普通 HTTP 转发（两种都要，`http.Client` 走代理时两种都会出现）
- 域名匹配 → `Policy` 决策；`Ask` 暂时按 `Deny` 处理并记 `reason="ask-unsupported"`（审批联动下一轮做）
- 私网地址默认拦（`AllowPrivate=false`），含 `169.254.169.254`（云元数据）
- 环形缓冲保存 `LogEntry`，`Log()` 倒序返回
- 代理认证：`Proxy-Authorization: Basic base64("<sandboxID>:<nonce>")`，
  用 `Register()` 登记的 sandboxID 反查归属；未登记的流量标 `unowned` 并按默认策略处理
- **测试必须覆盖**：域名匹配（精确/通配/大小写/端口剥离）、私网拦截、策略优先级、环形缓冲不泄漏

### B 调度（`schedule.Open`）
- 5 段 cron 解析（`*`, `*/n`, `a,b,c`, `a-b`），支持时区；**不要引 cron 库**
- `Store` 用单文件 JSON（`<dataDir>/schedules.json`）+ `flock` 风格的文件锁，
  进程内加 `sync.Mutex`；每次写用临时文件 + rename
- `Tick` 扫 `Due()` → 调 `Runner.CreateRun` → 更新 `LastRunAt/LastRunID/NextRunAt`
- **防重复触发**：同一 schedule 用 `LeaseTTL` 内的 `NextRunAt` 前移做乐观锁
- webhook token：`crypto/rand` 32 字节 base64url，创建时生成
- **测试必须覆盖**：cron 解析（含跨月/闰年/`*/0` 非法）、推进 `NextRunAt` 幂等、按租户配额

### C 沉淀（`distill.Open`）
- 读 `store.Store` 的 run + 事件流（`ListEvents`），按 `tool_call_start/update` 还原 `Steps`
- 生成 SKILL.md：frontmatter（name/description/version）+「何时使用」+「步骤」+「注意」
- **规则式，不调 LLM**：步骤、命令、路径都是事实，直接排版；措辞用固定模板
- 至少 2 步工具调用才允许沉淀，否则返回明确错误（纯问答没有可复用步骤）
- `Accept` 写到 `<SkillDir>/<name>/SKILL.md`，同名先备份成 `.bak.<时间戳>`
- 草稿存 `<dataDir>/drafts.json`
- **测试必须覆盖**：事件→步骤还原（含 failed/denied）、少于 2 步拒绝、同名备份、frontmatter 合法

### D 连接器（`connector.Open`）
- Vault：AES-256-GCM，key 从 `Config.VaultKey` base64 解；文件 0600
- 内置四个：github / google / notion（oauth2）+ 一个 apikey 型（比如 `openai`）
- `state` 必须**自包含**：`base64url(tenant|user|name|ts) + HMAC`，10 分钟过期
- OAuth 换 token 用标准 `application/x-www-form-urlencoded`
- `Inject`：`proxy` 模式返回空 map + 提示未实现（下一轮接 egress 代理）；
  `env` 模式返回 `{ENV_NAME: token}`，并在日志里**明确告警**
- **测试必须覆盖**：加解密往返、篡改密文报错、state 过期/伪造、token 刷新边界

### E 浏览器（`browser.Open`）
- 探测 Chrome 路径（macOS / Linux / Windows 各一条），起 `--headless=new --remote-debugging-port=0 --user-data-dir=<tmp>`
- 从 stderr 解析 `DevTools listening on ws://...`
- 每实例独立 `--user-data-dir`（用完即删，保证 cookie 隔离——这是 Grok Bot 被点名的问题）
- `MaxSessions` 超限返回 `ErrBusy`；`IdleTimeout` 清理
- 进程退出时 `CloseAll()`，并 kill 子进程（避免僵尸 Chrome）
- **测试必须覆盖**：解析 DevTools 行、并发上限、Close 后不再出现在 List

## 四、统一验收

1. `go build ./...`、`go vet ./<你的包>/`、`go test ./<你的包>/ -race -count=1` 全绿
2. `gofmt -l` 对你改的文件为空
3. 至少一条**真跑通**的证据（不是 mock）；起不来的（如 Chrome）要说清原因
4. 不新增第三方依赖（标准库够用；确实需要时先汇报）
5. 崩溃/超时/并发都要有明确的错误路径，不许 panic 到 HTTP 层
