# CLI 使用文档

`astra-cli`（面向用户与 agent）与 `admin-cli`（面向运营）是两个独立二进制，
共用 `internal/client` 这一套控制面客户端。命令表定稿于
[`docs/design/platform-extension.md`](./design/platform-extension.md) 第五节。

- 只依赖 Go 标准库（**没有引入 cobra**，`go.mod` 未改动）。
- 输出约定：默认人类可读表格；`--json` 输出原始 JSON（给脚本/agent）。
- 退出码：`0` 成功 · `1` 执行失败 · `2` 用法错误。

---

## 1. 构建

```bash
go build -o bin/astra-cli ./cmd/astra-cli
go build -o bin/admin-cli ./cmd/admin-cli
```

单二进制，无运行时依赖。Windows/macOS/Linux 同一套代码（路径分隔符统一走
`path/filepath`，配置文件权限在 Windows 上被忽略但不报错）。

## 2. 登录与配置

```bash
# 已有 token（JWT 或 API key）
astra-cli login --server http://127.0.0.1:7788 --token sk-xxx

# 用账号密码换 token（服务端的 login 需要租户 slug，默认 default）
astra-cli login --server http://127.0.0.1:7788 \
  --email 929803678@qq.com --password ******** --tenant default
```

配置落在 `~/.astra/cli.json`，权限 **0600**：

```json
{ "server": "http://127.0.0.1:7788", "token": "eyJ...", "email": "a@b.com", "tenant": "default" }
```

`admin-cli` 用**另一个文件** `~/.astra/admin-cli.json`。这不是洁癖：
两套凭据分开，才不会出现「拿用户 token 去停租户」或「把 admin token 留在
用户侧配置里」。服务端仍然按角色鉴权，本地只是不给误用制造便利。

优先级：`--server/--token` > `ASTRA_SERVER/ASTRA_TOKEN` > 配置文件。

`astra-cli logout` 直接删本地文件（JWT 无状态，本地无凭据即登出）。

## 3. astra-cli 命令

### 身份

```bash
astra-cli whoami          # 服务端地址 + 邮箱/角色/租户 + 能力开关
astra-cli whoami --json   # {"server": "...", "meta": {...}}
```

### 会话

```bash
astra-cli session list                 # ID / 标题 / 状态 / cwd / 最近活跃
astra-cli session new "排查构建失败"    # 返回会话 ID
astra-cli session rm <id> --yes        # 删会话 + 销毁沙箱（不可恢复，默认要确认）
```

### 提交任务

```bash
astra-cli run "echo hi" --wait
astra-cli run "跑 go test ./... 并总结" --wait --timeout 10m --session <id>
astra-cli run "长任务" --json          # 只提交，输出 run JSON
astra-cli run logs <run-id>            # 跟随事件流（Ctrl-C 断开）
astra-cli run logs <run-id> --after 42 # 从 seq 42 之后续传
astra-cli run cancel <run-id>
```

`--wait` 的流式输出规则：

| 事件 | 输出 |
|---|---|
| `message_delta` | 原样打到 **stdout**（可重定向成文件） |
| `tool_call_start` | `→ 工具名(入参摘要)` |
| `approval_req`（pending） | `! 需要审批：<标题>` + 审批命令提示 |
| `run_state` | 终态事件，流到此结束 |
| 其他（state / agent_log / tool_call_update） | 仅 `-v` 时打到 stderr |

退出码由 run 的终态决定：`completed=0`、`failed/cancelled=1`。
`--wait` 超时也返回 1，但**任务没死**——用 `run logs` 续上，不要重新提交。

### 审批

```bash
astra-cli approvals <run-id>                                  # 待审批列表
astra-cli approve <run-id> <tool-call-id> --allow
astra-cli approve <run-id> <tool-call-id> --deny
```

### 工作区文件

```bash
astra-cli files ls <session> [路径]          # 目录在前，含大小/时间
astra-cli files cat <session> <路径>         # 预览：512KB 截断、二进制不返内容
astra-cli files download <session> <路径> [--out 本地路径]
```

`cat` 与 `download` 打的是不同端点，**落盘一律用 `download`**：
`cat` 的内容可能被截断，二进制还会是空的。`download` 先写 `.part` 再改名，
失败不会留下半个文件。

### 长期记忆

```bash
astra-cli memory search "上次那个方案" --k 8           # 表格：SCORE/KIND/CREATED/TEXT
astra-cli memory search "部署 方案" --k 8 --json      # 原始 items
astra-cli memory add "所有 CLI 命令必须支持 --json" --kind fact
astra-cli memory stats                               # 条数/维度/embedder，不含内容
astra-cli memory rm <id> [--yes]
```

**没有**任何指定 `user_id` 的参数：服务端按调用者的 `tenant_id + user_id`
强制过滤，跨用户读取在设计上不存在。`--k` 上限 50（超出由服务端截断为 50）。

### Skills

```bash
astra-cli skills list                     # NAME/VERSION/REQUIRES/DESCRIPTION
astra-cli skills show astra-memory        # 打印 SKILL.md 原文
astra-cli skills install astra-memory     # 复制到 ~/.astra/skills/astra-memory
astra-cli skills install astra-run --dest ./skills/astra-run --force
astra-cli skills list --from ./skills     # 显式指定 skills 根目录
```

`list/install` 默认从当前目录向上找仓库里的 `skills/`（最多 8 层），
找不到就要求 `--from`。`install` 是整目录拷贝，目标已存在时需 `--force`。

## 4. admin-cli 命令

```bash
admin-cli login --server http://127.0.0.1:7788 \
  --email admin@example.com --password ******** --as admin
admin-cli whoami
```

### 租户

```bash
admin-cli tenant list
admin-cli tenant create --name "Acme" --slug acme --limit 1000000
admin-cli tenant suspend <id> [--yes]      # 危险：全租户立即 403
admin-cli tenant resume <id>
```

`tenant list` 的用量列来自 `/v1/admin/usage`：普通租户只看得到自己那一行，
其它租户显示 `-`（服务端不允许横向看账）。没有用量权限时显示 `n/a`，
**不会**假装是 0。

### 用户

```bash
admin-cli user list
admin-cli user create --email a@b.com --name 张三 --role member
admin-cli user reset-password <id> [--password 新密码] [--yes]
```

`user list` 只列当前身份所在租户（服务端无跨租户参数）；`--tenant` 只用于
校验，传入不匹配的租户会在本地被拦下。`reset-password` 会让该用户全部 token
立即失效，未指定密码时本地生成 16 位随机密码并只显示一次。

### 配额

```bash
admin-cli quota show [--tenant <id>]         # 上限 / 已用 / 剩余 / 花费
admin-cli quota set --tenant <id> --limit 1000000   # 0 = 不限
```

### 凭据

```bash
admin-cli key list                    # 只有前缀，明文不可再取
admin-cli key rotate [--name 名称] [--revoke-old] [--days 90]
```

`rotate` 默认**不**撤销旧凭据：先拿到新明文、验证能用，再加 `--revoke-old`，
避免「新钥匙有问题、旧的已经没了」把自己锁在门外。

### 沙箱

```bash
admin-cli sandbox list
admin-cli sandbox kill <id> [--yes]   # 只销毁运行环境，会话记录保留
```

### 审计

```bash
admin-cli audit tail                        # 最近 50 条，旧 → 新
admin-cli audit tail --follow                # 每 2s 轮询增量（Ctrl-C 退出）
admin-cli audit tail --action session.create --from 2026-11-01 --limit 200
```

### 观测与记忆

```bash
admin-cli obs status                  # enabled / endpoint / dashboard
admin-cli obs traces --run <run-id> --limit 20
admin-cli memory stats [--tenant <id>]  # 只读统计，不含任何记忆内容
```

`obs status` 在未启用时会直接列出需要配置的环境变量
（`LANGFUSE_HOST` / `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` /
`LANGFUSE_DASHBOARD_URL` / `ASTRA_AGENT_NAME`），不用去翻文档。

`memory stats` 是**集合级**统计：服务端没有按租户拆分的接口，
`--tenant` 只用于核对口径并在输出里说明这一点。

### 日志查询（`logs`）

后端把「能查历史」的端点散在七处，`logs` 分组把它们收在一起。与
`audit tail` 的分工：audit 记的是「谁调了什么管理接口」，logs 记的是
「一次执行 / 一次出网 / 一笔用量留下了什么」。

```bash
admin-cli logs egress [--limit 100] [--tenant <id>]     # 出网 allow/deny/审批记录
admin-cli logs history <runID> [--after N]              # run 完整事件历史（非 SSE）
admin-cli logs activity <runID> [--limit 200]           # 行动账本：做了什么、卡在哪
admin-cli logs activity --session <sid> [--limit 200]   # 按会话聚合多个 run
admin-cli logs children <runID>                        # 该 run 派生的子 agent
admin-cli logs approvals <runID>                       # 待决/已决的人工审批
admin-cli logs usage [--from T] [--to T] [--limit N] [--user <id>]
admin-cli logs billing                                 # 本月账单
```

- 时间参数接受 RFC3339 或纯日期（按 UTC 当天 0 点）。
- `logs activity` 的位置参数等价于 `--run`；`--run` 与 `--session` 二选一，
  都不给会直接报用法错误（退出码 2），不会发出一个路径不完整的请求。
- `logs usage` 走 `/v1/usage`（成员可查自己），`quota show` 走
  `/v1/admin/usage`（治理视角），两者别混。
- `logs egress` 需要 `audit:read`；能力未启用时服务端回 503，CLI 会原样显示。
- 全局 flag 可以写在子命令前（`logs --json egress`），CLI 会把值型 flag
  剥掉再识别子命令；其余位置随意。

常用的组合：先 `logs activity <runID>` 看用户视角的时间线，觉得哪一步可疑
再 `logs history <runID>` 翻原始事件，最后 `logs egress` 对照出网记录。

### 远程桌面（`remote`）

手机看/控这台电脑。命令只做「电脑跟前的那个人」能做的事：出配对码、看设备、
批准接管、急停；画面与手势在手机上（WebSocket + JPEG 帧），命令行不碰。
用户向说明见 [`docs/remote-desktop.md`](./remote-desktop.md)。

```bash
admin-cli remote capabilities [--strict]              # 三层能力：监看 / 快捷操作 / 键鼠接管
admin-cli remote pair [--device NAME]                 # 6 位配对码（120 秒、一次性）
admin-cli remote claim <code> [--device NAME]         # 用配对码把本机绑成设备（脚本/手机侧）
admin-cli remote devices                              # 设备列表：在线 / 最后活跃 / 已吊销
admin-cli remote revoke <deviceID> [--yes]            # 吊销设备（级联断开其会话）
admin-cli remote start --device ID [--mode view|control]
admin-cli remote sessions [--limit 20]                # 状态 / 帧率 / 掉帧 / 时长
admin-cli remote approve <sessionID>                  # 批准接管（pending → active）
admin-cli remote end <sessionID>                      # 急停：立即断开
admin-cli remote action <sessionID> <动作> [参数]      # open-app / open-url / screenshot / lock
```

- **退出码**：只有「监看不可用」算失败（`1`）——没有画面这个能力就不存在。
  快捷操作 / 键鼠接管不可用只是降级（只读模式），返回 `0`；加 `--strict`
  则任意一层不可用都返回 `1`，给部署自检用。
- `capabilities` 的输出是三层各一行：能用的打 `OK`（附后端名），
  不能用的打原因 **和修复路径**（例如「系统设置 → 隐私与安全 → 辅助功能」）。
- `pair` 把 `payload` 单独一行输出，方便复制给二维码工具或原生 App 深链。
- `start --mode control` 默认落在 `pending_approval`，CLI 会直接打印
  「等待桌面确认」并给出 `remote approve` 提示；批准前手机只能看画面。
- `action` 的动作名是 CLI 侧的连字符写法，映射到协议的下划线值：
  `open-app`→`open_app`（参数是应用名）、`open-url`→`open_url`（仅 http/https）、
  `screenshot`（返回电脑上的落盘路径）、`lock`。
- `end` 是急停，**不做二次确认**（它存在的场景就是「现在必须断」）；
  `revoke` 会断开会话，所以要确认，`--yes` / `--json` 跳过。
- 时间列一律 UTC 并在表头标注，`formatTime` 输出带 `UTC` 字样，不会被本地时区平移。
- 能力未启用（`ASTRA_REMOTE_DISABLED=1` 或装配失败）时服务端回 503，
  CLI 原样显示原因，不会伪装成「空列表」。

电脑端的自检用 `make remote-check`：抓屏 / 注入 / 控制面能力接口三行，
抓屏不可用返回 1，注入不可用只记警告。

## 5. 脚本化

```bash
# 拿 run id
RUN=$(astra-cli run "build" --json | jq -r .id)

# 等到结束（退出码即结论）
astra-cli run logs "$RUN" > /dev/null || echo "任务失败"

# 记忆检索结果喂给 jq
astra-cli memory search "部署" --k 5 --json | jq -r '.[] | "\(.score|tostring[0:5])  \(.text)"'

# 审计落地
admin-cli audit tail --limit 1000 --json > audit.json

# 出网拒绝记录 + 某个 run 的完整事件流
admin-cli logs egress --limit 500 --json > egress.json
admin-cli logs history "$RUN" --json > events.json

# 远程桌面：先确认本机能看能控，再给手机出码
admin-cli remote capabilities --strict --json | jq '.capture.available'
admin-cli remote pair --json | jq -r .code
```

`--json` 输出的就是服务端原样结构，字段名不加壳、不改名——脚本依赖的
是 API 契约，不是 CLI 的措辞。

## 6. 排障

**Q：`错误：需要登录（HTTP 401）`**
token 过期。重新 `login`；如果是 API key，用 `admin-cli key list` 看是否已被撤销。

**Q：`... 返回的不是 JSON（Content-Type: text/html）`**
服务端没有这个路由，被 SPA 的 `GET /` 兜底返回了首页。通常是服务端版本较旧
（例如 `files download`、`memory stats`、`obs status` 需要较新的控制面）。

**Q：`--wait` 一直不结束**
run 可能停在 `waiting_approval`（stderr 会打 `· 等待人工审批`）。
用 `astra-cli approvals <run-id>` 看，再 `approve`。

**Q：`文件不存在` / `越界`（403）**
`files` 的路径是**沙箱工作区内的相对路径**，不接受绝对路径与 `..`。

**Q：表格对不齐**
只影响观感：宽度按「CJK 字符占两格」计算，个别 emoji 可能偏一格。
用 `--json` 可以完全绕开排版。

**Q：想看每个请求**
加 `-v`（或 `--verbose`），会打印 `→ GET /v1/...` 与状态码/耗时到 stderr。
