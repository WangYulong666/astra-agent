package clikit

// CommandSpec 是一条 CLI 原子能力的机器可读描述。
//
// 以前两个 CLI 的命令表是 main.go 里的 Go map + printMainHelp 的硬编码文本，
// agent 无从查询「有哪些命令、各自要什么能力」。这里作为唯一事实来源，
// 能力注册表（internal/capability）和 help 输出都从它生成。
type CommandSpec struct {
	CLI         string   // astra-cli | admin-cli
	Name        string   // 完整命令名，如 "memory search" / "logs egress"
	Usage       string   // 带参数占位的一行用法
	Description string   // 一句话说明
	Requires    []string // 依赖的能力开关，如 ["memory"]、["egress"]
}

// KnownCommands 返回全部已登记命令。由 T1 填充；新增子命令必须同步这里，
// 否则 TestCLIRegistryCoversCommands 会红（防止能力清单悄悄漏项）。
func KnownCommands() []CommandSpec { return knownCommands }
