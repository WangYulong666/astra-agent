// admin-cli 是运营/运维命令行。
//
// 与 astra-cli 分开是一个安全设计：两边配置各自独立（admin-cli.json vs cli.json），
// 避免「用低权限 token 跑管理命令」或把 admin token 留在用户侧配置里。
// 真正的鉴权仍在服务端（角色决定能不能做），本地只负责不给误用制造便利。
//
// 命令表见 docs/design/platform-extension.md 第五节。
// 退出码：0 成功 / 1 执行失败 / 2 用法错误。
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

const configName = "admin-cli.json"

type globals struct {
	json    bool
	server  string
	token   string
	verbose bool
}

type app struct {
	out     io.Writer
	err     io.Writer
	in      io.Reader
	g       globals
	cfg     clikit.Config
	cfgPath string
	// yes 是全局的「跳过二次确认」。放在 app 上而不是每个命令的 flag 里，
	// 是为了让 --yes 的语义在所有危险命令上完全一致。
	yes bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin))
}

func run(args []string, out, errW io.Writer, in io.Reader) int {
	a := &app{out: out, err: errW, in: in}
	if p, err := clikit.Path(configName); err == nil {
		a.cfgPath = p
		if c, e := clikit.LoadConfig(p); e == nil {
			a.cfg = c
		}
	}
	cmd, sub, rest := splitCommand(args)
	switch cmd {
	case "", "help", "-h", "--help":
		a.printMainHelp()
		// 显式 --help 是 0；什么都没给才算用法错误。
		if cmd == "" && !wantsHelp(args) {
			return 2
		}
		return 0
	case "version", "--version":
		fmt.Fprintf(out, "admin-cli %s\n", version)
		return 0
	case "login":
		return a.cmdLogin(rest)
	case "whoami":
		return a.cmdWhoami(rest)
	case "tenant":
		return a.cmdTenant(sub, rest)
	case "user":
		return a.cmdUser(sub, rest)
	case "quota":
		return a.cmdQuota(sub, rest)
	case "key":
		return a.cmdKey(sub, rest)
	case "sandbox":
		return a.cmdSandbox(sub, rest)
	case "audit":
		return a.cmdAudit(sub, rest)
	case "obs":
		return a.cmdObs(sub, rest)
	case "memory":
		return a.cmdMemory(sub, rest)
	default:
		fmt.Fprintf(errW, "未知命令：%s\n\n", cmd)
		a.printMainHelp()
		return 2
	}
}

var knownCommands = map[string]bool{
	"login": true, "whoami": true, "tenant": true, "user": true, "quota": true,
	"key": true, "sandbox": true, "audit": true, "obs": true, "memory": true,
	"version": true, "help": true, "-h": true, "--help": true, "--version": true,
}

// groupCommands 是「下面还有子命令」的命令。只有它们才把下一个词当子命令，
// 否则 `user reset-password <id>` 里的 id 会被当成子命令吃掉。
var groupCommands = map[string]bool{
	"tenant": true, "user": true, "quota": true, "key": true,
	"sandbox": true, "audit": true, "obs": true, "memory": true,
}

// splitCommand 与 astra-cli 同构：从左扫到第一个已知命令名作为锚点，
// 其余原样交给子命令解析（这样 `--token xxx tenant list` 与
// `tenant list --token xxx` 都能用）。
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
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return a, "", args
			}
		}
		return "", "", args
	}
	rest = append(rest, args[:idx]...)
	j := idx + 1
	if groupCommands[cmd] && j < len(args) && !strings.HasPrefix(args[j], "-") {
		sub = args[j]
		j++
	}
	rest = append(rest, args[j:]...)
	return cmd, sub, rest
}

func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.err)
	fs.BoolVar(&a.g.json, "json", a.g.json, "输出原始 JSON")
	fs.StringVar(&a.g.server, "server", a.g.server, "控制面地址，默认取配置")
	fs.StringVar(&a.g.token, "token", a.g.token, "访问 token，默认取配置")
	fs.BoolVar(&a.g.verbose, "v", a.g.verbose, "打印请求明细到 stderr")
	fs.BoolVar(&a.g.verbose, "verbose", a.g.verbose, "同 -v")
	fs.BoolVar(&a.yes, "yes", false, "危险操作跳过二次确认")
	fs.Usage = func() {
		fmt.Fprintf(a.err, "用法：admin-cli %s [flags]\n\nflags：\n", name)
		fs.PrintDefaults()
	}
	return fs
}

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
		return nil, &client.UseError{Msg: "未配置服务端地址：admin-cli login --server http://127.0.0.1:7788 --token <admin-token>"}
	}
	c := client.New(server, token)
	if a.g.verbose {
		c.SetLogger(func(format string, args ...any) {
			fmt.Fprintf(a.err, "[v] "+format+"\n", args...)
		})
	}
	return c, nil
}

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

func (a *app) jsonOut(v any, human func() error) int {
	if a.g.json {
		if err := clikit.PrintJSON(a.out, v); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if human != nil {
		if err := human(); err != nil {
			return a.fail(err)
		}
	}
	return 0
}

// confirm 危险操作的二次确认。--yes 或 --json 时跳过（--json 是脚本场景，
// 脚本要的是确定性；这时候要人工输入等于把管道卡死）。
func (a *app) confirm(prompt string) bool {
	if a.yes {
		return true
	}
	if a.g.json {
		return true
	}
	return clikit.Confirm(a.in, a.out, prompt)
}

func wantsHelp(args []string) bool {
	for _, x := range args {
		if x == "-h" || x == "--help" {
			return true
		}
	}
	return false
}

func (a *app) printMainHelp() {
	p := a.cfgPath
	if p == "" {
		p = "~/.astra/" + configName
	}
	fmt.Fprintf(a.out, `admin-cli %s — Astra Agent 运营命令行

用法：
  admin-cli <命令> [子命令] [参数] [flags]

命令：
  login --server URL [--token T | --email E --password P] [--as admin]
  whoami                                                     当前身份
  tenant list                                                租户 + 用量 + 状态
  tenant create --name N --slug S [--limit 1000000]          新建租户
  tenant suspend <租户ID> [--yes]                             停用租户（危险）
  tenant resume <租户ID>                                     恢复租户
  user list                                                  当前租户用户
  user create --email E [--role member] [--name 显示名]       新建用户
  user reset-password <用户ID> [--password 新密码] [--yes]     重置密码（危险）
  quota show [--tenant ID]                                   配额与本月用量
  quota set --tenant ID --limit 1000000                      改月度 token 上限（0=不限）
  key list                                                   长期凭据列表（只有前缀）
  key rotate [--name 名称] [--revoke-old]                     签发新凭据（明文只出现一次）
  sandbox list                                               运行中的沙箱
  sandbox kill <沙箱ID> [--yes]                               强杀沙箱（危险）
  audit tail [--follow] [--limit 50] [--action X]            审计日志
  obs status                                                 观测能力状态与所需变量
  obs traces [--run RUN_ID] [--limit 20]                     trace 反查
  memory stats [--tenant ID]                                 记忆只读统计（不含内容）
  version                                                    版本

全局 flags（可写在命令前后）：
  --json        输出原始 JSON
  --server URL  控制面地址（默认取配置）
  --token T     访问 token（默认取配置）
  -v            打印请求明细
  --yes         跳过危险操作的二次确认

配置文件：%s（0600，与 astra-cli 的 cli.json 分开）
退出码：0 成功 · 1 执行失败 · 2 用法错误
`, version, p)
}
