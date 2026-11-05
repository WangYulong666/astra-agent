// astra-cli 是面向用户/agent 的命令行入口。
//
// 与 admin-cli 的分界：这里只做「我的会话、我的任务、我的记忆、我的文件」，
// 没有任何跨租户视角；运营相关的读写在 admin-cli。两个二进制各自存配置，
// 就是为了让「拿用户 token 去停租户」这件事做不到，而不是靠自觉。
//
// 命令表与退出码来自 docs/design/platform-extension.md 第五节：
//
//	0 成功 / 1 执行失败 / 2 用法错误
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const version = "1.0.0"

// 配置文件名。与 admin-cli 分开是安全设计，不是洁癖（见包注释）。
const configName = "cli.json"

// globals 是所有子命令共享的开关。放在一个结构里而不是包级变量，
// 是为了让 run() 可被测试重复调用而不会串状态。
type globals struct {
	json    bool
	server  string
	token   string
	verbose bool
}

type app struct {
	out     io.Writer
	err     io.Writer
	g       globals
	cfg     clikit.Config
	cfgPath string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errW io.Writer) int {
	a := &app{out: out, err: errW}
	p, err := clikit.Path(configName)
	if err == nil {
		a.cfgPath = p
		if c, e := clikit.LoadConfig(p); e == nil {
			a.cfg = c
		}
	}
	cmd, sub, rest := splitCommand(args)
	switch cmd {
	case "", "help", "-h", "--help":
		a.printMainHelp()
		// 显式 --help 是 0；什么都没给（或只有 flag）才算用法错误。
		if cmd == "" && !wantsHelp(args) {
			return 2
		}
		return 0
	case "version", "--version":
		fmt.Fprintf(out, "astra-cli %s\n", version)
		return 0
	case "login":
		return a.cmdLogin(rest)
	case "logout":
		return a.cmdLogout(rest)
	case "whoami":
		return a.cmdWhoami(rest)
	case "session":
		return a.cmdSession(sub, rest)
	case "run":
		return a.cmdRun(sub, rest)
	case "files":
		return a.cmdFiles(sub, rest)
	case "approve":
		return a.cmdApprove(rest)
	case "approvals":
		return a.cmdApprovals(rest)
	case "memory":
		return a.cmdMemory(sub, rest)
	case "skills":
		return a.cmdSkills(sub, rest)
	default:
		fmt.Fprintf(errW, "未知命令：%s\n\n", cmd)
		a.printMainHelp()
		return 2
	}
}

// ── 参数解析 ─────────────────────────────────────────────────

// globalTakesValue 保留给帮助文本的说明；命令切分不再依赖它。
var knownCommands = map[string]bool{
	"login": true, "logout": true, "whoami": true, "session": true, "run": true,
	"files": true, "approve": true, "approvals": true, "memory": true,
	"skills": true, "version": true, "help": true, "-h": true, "--help": true,
	"--version": true,
}

// groupCommands 是「下面还有子命令」的命令。只有它们才把下一个词当子命令：
// `approve r1 tc1` 里的 r1 是参数，不是子命令——早期版本就是这么错的。
var groupCommands = map[string]bool{
	"session": true, "run": true, "files": true, "memory": true, "skills": true,
}

// splitCommand 找出「命令 + 子命令」，剩下的全部原样交给子命令的 FlagSet。
//
// 为什么不是「第一个非 flag 词就是命令」：`login --email a@b --password x`
// 里 --email 是 flag，但它不是全局 flag，切分器分不清后面那个值是不是命令。
// 改成「从左扫到第一个已知命令名」就没有这个问题——命令名是有限集合，
// 而密码/地址/提示词都是任意文本，只有前者能当锚点。
//
// 代价：`astra-cli run "session"` 这种「提示词恰好等于命令名」的输入，
// 因为 run 在前，仍然解析成 run——锚点取最左边，所以是安全的。
func splitCommand(args []string) (cmd, sub string, rest []string) {
	idx := -1
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if knownCommands[a] {
			idx, cmd = i, a
			break
		}
	}
	if idx < 0 {
		// 没有已知命令：把第一个非 flag 词当命令，好让上层报「未知命令：xxx」。
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return a, "", args
			}
		}
		return "", "", args
	}
	rest = append(rest, args[:idx]...)
	j := idx + 1
	// 只有分组命令才吃下一个词当子命令；其余命令的「下一个词」是它的参数。
	if groupCommands[cmd] && j < len(args) && !strings.HasPrefix(args[j], "-") {
		sub = args[j]
		j++
	}
	rest = append(rest, args[j:]...)
	return cmd, sub, rest
}

// newFlagSet 建一个带全局开关、且不自动打印错误的 FlagSet。
// ContinueOnError：参数错时要走我们的退出码 2，而不是让 flag 直接 os.Exit(2)。
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.err)
	fs.BoolVar(&a.g.json, "json", a.g.json, "输出原始 JSON（给脚本/agent 用）")
	fs.StringVar(&a.g.server, "server", a.g.server, "控制面地址，默认取配置文件")
	fs.StringVar(&a.g.token, "token", a.g.token, "访问 token，默认取配置文件")
	fs.BoolVar(&a.g.verbose, "v", a.g.verbose, "打印请求明细到 stderr")
	fs.BoolVar(&a.g.verbose, "verbose", a.g.verbose, "同 -v")
	fs.Usage = func() {
		fmt.Fprintf(a.err, "用法：astra-cli %s [flags]\n\nflags：\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// wantsHelp 判断参数里有没有 -h/--help。分组命令（session / run / files …）
// 没有自己的 FlagSet，得自己认，否则 `session --help` 会被当成「没有子命令」报错。
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parse 解析参数：先把 flag 与位置参数分开（支持 flag 写在位置参数之后），
// 再交给 flag 包解析。返回位置参数与退出码（>=0 表示应当直接退出）。
func (a *app) parse(fs *flag.FlagSet, args []string, takesValue map[string]bool) ([]string, int, bool) {
	flags, pos := clikit.SplitFlags(args, takesValue)
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, true
		}
		return nil, 2, true
	}
	return pos, 0, false
}

// api 按「flag > 环境变量 > 配置文件」的优先级构造客户端。
func (a *app) api() (*client.Client, error) {
	server := a.g.server
	if server == "" {
		server = os.Getenv("ASTRA_SERVER")
	}
	if server == "" {
		server = a.cfg.Server
	}
	token := a.g.token
	if token == "" {
		token = os.Getenv("ASTRA_TOKEN")
	}
	if token == "" {
		token = a.cfg.Token
	}
	if server == "" {
		return nil, &client.UseError{Msg: "未配置服务端地址：astra-cli login --server http://127.0.0.1:7788 --token <token>"}
	}
	c := client.New(server, token)
	if a.g.verbose {
		c.SetLogger(func(format string, args ...any) {
			fmt.Fprintf(a.err, "[v] "+format+"\n", args...)
		})
	}
	return c, nil
}

// fail 把错误映射到退出码：用法错误 2，其余 1。
func (a *app) fail(err error) int {
	fmt.Fprintf(a.err, "错误：%v\n", err)
	if client.IsUseError(err) {
		return 2
	}
	return 1
}

func (a *app) needArgs(pos []string, n int, usage string) error {
	if len(pos) < n {
		return &client.UseError{Msg: "参数不足\n用法：" + usage}
	}
	return nil
}

// jsonOut 统一 --json 行为：开了就打原始 JSON，没开就调 human（打印表格等）。
// 返回值直接当退出码用，省掉每个命令里一层 if err != nil。
func (a *app) jsonOut(v any, human func() error) int {
	if a.g.json {
		if err := clikit.PrintJSON(a.out, v); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if human == nil {
		return 0
	}
	if err := human(); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *app) printMainHelp() {
	p := a.cfgPath
	if p == "" {
		p = "~/.astra/" + configName
	}
	fmt.Fprintf(a.out, `astra-cli %s — Astra Agent 用户命令行

用法：
  astra-cli <命令> [子命令] [参数] [flags]

命令：
  login --server URL [--token T | --email E --password P]   登录并保存凭据
  logout                                                    删除本地凭据
  whoami                                                    当前身份与服务信息
  session list                                              列出会话
  session new [标题]                                         新建会话
  session rm <会话ID> [--yes]                                删除会话（销毁沙箱）
  run "指令" [--session ID] [--wait] [--timeout 5m]          提交任务，--wait 跟随到终态
  run logs <run-id>                                         跟随事件流（Ctrl-C 退出）
  run cancel <run-id>                                       取消任务
  approvals <run-id>                                        列出待审批项
  approve <run-id> <tool-call-id> --allow|--deny             人工审批
  files ls <会话ID> [路径]                                   列出工作区文件
  files cat <会话ID> <路径>                                  直接输出文件内容
  files download <会话ID> <路径> [--out 本地路径]             下载文件
  memory search "..." [--k 10] [--kind fact|conversation]    检索长期记忆
  memory add "..." [--kind fact] [--session ID]              写入一条记忆
  memory stats                                              记忆统计（不含内容）
  skills list                                               列出内置 skill
  skills show <名称>                                         打印 SKILL.md
  skills install <名称> [--dest DIR] [--from DIR]            安装 skill 到本地目录
  version                                                   打印版本

全局 flags（可写在命令前后）：
  --json              输出原始 JSON
  --server URL        控制面地址（默认取配置）
  --token T           访问 token（默认取配置）
  -v, --verbose       打印请求明细

配置文件：%s（权限 0600）
环境变量：ASTRA_SERVER / ASTRA_TOKEN（优先级低于命令行 flag）

退出码：0 成功 · 1 执行失败 · 2 用法错误
`, version, p)
}
