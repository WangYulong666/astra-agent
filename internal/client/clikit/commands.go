package clikit

// knownCommands 是两份 CLI 命令表的唯一事实来源：能力注册表
// (internal/capability) 的 cli 类条目由它生成，两个 CLI 的 main help
// 也应从它生成（消灭硬编码文本）。
//
// 登记规则：
//   - Name 用完整命令名，分组命令写到子命令一级（"memory search"），
//     顶层单命令就写单词（"whoami"）。别名（ls/find/log/...）不另立条目，
//     只在 Usage 里体现规范写法；防漂移测试按 case 行覆盖别名。
//   - Usage 抄帮助文本的一行用法，带 CLI 名与参数占位。
//   - Requires 只填能力开关名，无依赖留 nil。取值必须落在
//     allowedRequires 集合内（见 registry_test.go 的完整性断言）。
//
// 新增子命令务必同步这里，否则 TestCLIRegistryCoversCommands 会红。
var knownCommands = []CommandSpec{
	// ── astra-cli：用户 / agent 侧 ─────────────────────────────
	{"astra-cli", "login", `astra-cli login --server URL [--token T | --email E --password P]`, "登录并保存凭据", nil},
	{"astra-cli", "logout", `astra-cli logout`, "删除本地凭据", nil},
	{"astra-cli", "whoami", `astra-cli whoami`, "查看当前身份与服务信息", nil},
	{"astra-cli", "version", `astra-cli version`, "打印版本", nil},

	{"astra-cli", "session list", `astra-cli session list`, "列出会话", nil},
	{"astra-cli", "session new", `astra-cli session new [标题]`, "新建会话", nil},
	{"astra-cli", "session rm", `astra-cli session rm <会话ID> [--yes]`, "删除会话（连同沙箱销毁，不可恢复）", nil},

	{"astra-cli", "run", `astra-cli run "指令" [--session ID] [--wait] [--timeout 5m]`, "提交任务，--wait 跟随到终态", nil},
	{"astra-cli", "run logs", `astra-cli run logs <run-id> [--after SEQ]`, "跟随 run 事件流（Ctrl-C 退出）", nil},
	{"astra-cli", "run cancel", `astra-cli run cancel <run-id>`, "取消任务", nil},

	{"astra-cli", "approvals", `astra-cli approvals <run-id>`, "列出待审批项", nil},
	{"astra-cli", "approve", `astra-cli approve <run-id> <tool-call-id> --allow|--deny`, "人工审批工具调用", nil},

	{"astra-cli", "files ls", `astra-cli files ls <会话ID> [路径]`, "列出工作区文件", nil},
	{"astra-cli", "files cat", `astra-cli files cat <会话ID> <路径>`, "直接输出文件内容（预览，二进制不返内容）", nil},
	{"astra-cli", "files download", `astra-cli files download <会话ID> <路径> [--out 本地路径]`, "下载文件到本地", nil},

	{"astra-cli", "memory search", `astra-cli memory search "关键词" [--k 10] [--kind fact|conversation]`, "检索长期记忆", []string{"memory"}},
	{"astra-cli", "memory add", `astra-cli memory add "内容" [--kind fact] [--session ID]`, "写入一条记忆", []string{"memory"}},
	{"astra-cli", "memory stats", `astra-cli memory stats`, "记忆统计（不含内容）", []string{"memory"}},
	{"astra-cli", "memory rm", `astra-cli memory rm <记忆ID> [--yes]`, "删除一条记忆（不可恢复）", []string{"memory"}},

	{"astra-cli", "skills list", `astra-cli skills list [--from DIR]`, "列出 skill（含不可用原因）", []string{"skills"}},
	{"astra-cli", "skills show", `astra-cli skills show <名称> [--from DIR]`, "打印 SKILL.md 正文", []string{"skills"}},
	{"astra-cli", "skills check", `astra-cli skills check [--from DIR]`, "校验每个 skill 的 requires，存在不可用项退出码 1", []string{"skills"}},
	{"astra-cli", "skills install", `astra-cli skills install <名称> [--dest DIR] [--from DIR] [--force]`, "安装 skill 到本地目录", []string{"skills"}},
	{"astra-cli", "skills drafts", `astra-cli skills drafts [--limit N]`, "列出 distill 待审草稿（走控制面）", []string{"skills"}},
	{"astra-cli", "skills accept", `astra-cli skills accept <草稿ID>`, "接受草稿：落盘 + 刷新索引与能力注册表", []string{"skills"}},
	{"astra-cli", "skills reject", `astra-cli skills reject <草稿ID>`, "拒绝草稿", []string{"skills"}},

	// ── admin-cli：运营 / 运维侧 ────────────────────────────────
	{"admin-cli", "login", `admin-cli login --server URL [--token T | --email E --password P] [--as admin]`, "登录并保存运营凭据", nil},
	{"admin-cli", "whoami", `admin-cli whoami`, "查看当前身份", nil},
	{"admin-cli", "version", `admin-cli version`, "打印版本", nil},

	{"admin-cli", "tenant list", `admin-cli tenant list`, "租户列表 + 用量 + 状态", nil},
	{"admin-cli", "tenant create", `admin-cli tenant create --name N --slug S [--limit 1000000]`, "新建租户", nil},
	{"admin-cli", "tenant suspend", `admin-cli tenant suspend <租户ID> [--yes]`, "停用租户（危险，全租户立即停服）", nil},
	{"admin-cli", "tenant resume", `admin-cli tenant resume <租户ID>`, "恢复租户", nil},

	{"admin-cli", "user list", `admin-cli user list [--tenant ID]`, "列出当前租户用户", nil},
	{"admin-cli", "user create", `admin-cli user create --email E [--role member] [--name 显示名]`, "新建用户", nil},
	{"admin-cli", "user reset-password", `admin-cli user reset-password <用户ID> [--password 新密码] [--yes]`, "重置用户密码（危险，踢下线）", nil},

	{"admin-cli", "quota show", `admin-cli quota show [--tenant ID]`, "查看配额与本月用量", nil},
	{"admin-cli", "quota set", `admin-cli quota set --tenant ID --limit 1000000`, "修改月度 token 上限（0=不限）", nil},

	{"admin-cli", "key list", `admin-cli key list`, "长期凭据列表（只有前缀）", nil},
	{"admin-cli", "key rotate", `admin-cli key rotate [--name 名称] [--revoke-old] [--days 90]`, "签发新凭据（明文只出现一次）", nil},

	{"admin-cli", "sandbox list", `admin-cli sandbox list`, "列出运行中的沙箱", nil},
	{"admin-cli", "sandbox kill", `admin-cli sandbox kill <沙箱ID> [--yes]`, "强杀沙箱（危险，会话记录保留）", nil},

	{"admin-cli", "audit tail", `admin-cli audit tail [--follow] [--limit 50] [--action X] [--from T] [--to T]`, "审计日志（管理接口调用）", nil},

	{"admin-cli", "logs egress", `admin-cli logs egress [--limit 100] [--tenant ID]`, "出网审计（allow/deny/审批）", []string{"egress"}},
	{"admin-cli", "logs history", `admin-cli logs history <runID> [--after N]`, "run 完整事件历史", nil},
	{"admin-cli", "logs activity", `admin-cli logs activity <runID> [--limit 200]`, "行动账本（做了什么 / 卡在哪）", []string{"activity"}},
	{"admin-cli", "logs children", `admin-cli logs children <runID>`, "子 agent 派生记录", []string{"collab"}},
	{"admin-cli", "logs approvals", `admin-cli logs approvals <runID>`, "待决 / 已决的人工审批", nil},
	{"admin-cli", "logs usage", `admin-cli logs usage [--from T] [--to T] [--limit N] [--user ID]`, "用量流水", nil},
	{"admin-cli", "logs billing", `admin-cli logs billing`, "本月账单", nil},
	{"admin-cli", "logs insights", `admin-cli logs insights [--limit N] [--session ID]`, "失败 run 的结构化诊断与建议", []string{"insights"}},

	{"admin-cli", "remote capabilities", `admin-cli remote capabilities [--strict]`, "远程桌面三层能力（监看/快捷操作/键鼠接管）", []string{"remote"}},
	{"admin-cli", "remote pair", `admin-cli remote pair [--device NAME]`, "生成手机配对码（120 秒、一次性）", []string{"remote"}},
	{"admin-cli", "remote claim", `admin-cli remote claim <code> [--device NAME]`, "用配对码把本机绑定为设备", []string{"remote"}},
	{"admin-cli", "remote devices", `admin-cli remote devices`, "已绑定的设备列表（在线/最后活跃/已吊销）", []string{"remote"}},
	{"admin-cli", "remote revoke", `admin-cli remote revoke <deviceID> [--yes]`, "吊销设备（级联断开其会话）", []string{"remote"}},
	{"admin-cli", "remote start", `admin-cli remote start --device ID [--mode view|control]`, "建远程桌面会话（control 需桌面端确认）", []string{"remote"}},
	{"admin-cli", "remote sessions", `admin-cli remote sessions [--limit 20]`, "会话列表（状态/帧率/掉帧/时长）", []string{"remote"}},
	{"admin-cli", "remote approve", `admin-cli remote approve <sessionID>`, "批准键鼠接管", []string{"remote"}},
	{"admin-cli", "remote end", `admin-cli remote end <sessionID>`, "急停：断开会话", []string{"remote"}},
	{"admin-cli", "remote action", `admin-cli remote action <sessionID> <动作> [参数]`, "快捷操作（开 App/网址/截屏/锁屏）", []string{"remote"}},

	{"admin-cli", "obs status", `admin-cli obs status`, "观测能力状态与所需环境变量", []string{"trace"}},
	{"admin-cli", "obs traces", `admin-cli obs traces [--run RUN_ID] [--limit 20]`, "trace 反查", []string{"trace"}},

	{"admin-cli", "memory stats", `admin-cli memory stats [--tenant ID]`, "记忆只读统计（集合级，不含内容）", []string{"memory"}},
}
