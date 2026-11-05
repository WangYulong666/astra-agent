// Command astra-sandbox 是一个沙箱管理 CLI，核心用途是**本地开发调试**。
//
// 设计意图：让「起一个能连的沙箱」变成一条命令，从而可以在 Mac 上跑通
// worker → acp-proxy → dsh 的完整链路，而不必先有一台 x86_64 + KVM 的机器。
//
//	astra-sandbox up dev            # 起沙箱，打印 WSS 端点
//	astra-sandbox ls                # 看有哪些沙箱活着
//	astra-sandbox endpoint dev      # 只打印 URL/token，便于管道
//	astra-sandbox rm dev            # 销毁
//	astra-sandbox doctor            # 检查当前 provider 的前置条件
//
// 然后就可以用 M0 的验证器直接连：
//
//	go run ./cmd/acp-smoke -url "$(astra-sandbox endpoint dev -field url)" \
//	    -token "$(astra-sandbox endpoint dev -field token)" -skip-prompt
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
)

type options struct {
	provider  string
	root      string
	proxyBin  string
	agent     string
	workspace string
	template  string
	cubeAPI   string
	cubeToken string
	token     string
	timeout   time.Duration
	strict    bool
	verbose   bool
}

func main() {
	var o options
	flag.StringVar(&o.provider, "provider", envOr("ASTRA_SANDBOX_PROVIDER", "local"),
		"沙箱后端：local / applecontainer / cube")
	flag.StringVar(&o.root, "root", os.Getenv("ASTRA_SANDBOX_ROOT"),
		"本地状态根目录（默认 ~/.astra/sandboxes）")
	flag.StringVar(&o.proxyBin, "proxy", os.Getenv("ASTRA_PROXY_BIN"),
		"acp-proxy 二进制路径（默认在 PATH 与 ./bin 里找）")
	flag.StringVar(&o.agent, "agent", defaultAgent(), "agent 启动命令（空格切分）")
	flag.StringVar(&o.workspace, "workspace", "", "工作目录（默认 <root>/<id>/workspace）")
	flag.StringVar(&o.template, "template", "", "镜像/模板（容器与 cube 用）")
	flag.StringVar(&o.cubeAPI, "cube-api", os.Getenv("ASTRA_CUBE_API"), "CubeAPI 地址")
	flag.StringVar(&o.cubeToken, "cube-token", os.Getenv("ASTRA_CUBE_TOKEN"), "CubeAPI 凭据")
	flag.StringVar(&o.token, "token", "", "acp-proxy 的 Bearer token（默认随机生成）")
	flag.DurationVar(&o.timeout, "timeout", 0, "沙箱存活上限，0 表示不限")
	flag.BoolVar(&o.strict, "strict", true, "local 下用 sandbox-exec 限制可写范围")
	flag.BoolVar(&o.verbose, "v", false, "打印详细过程")

	// 不能直接 flag.Parse()：Go 的 flag 包遇到第一个非 flag 参数就停止，
	// 而 `up dev -agent "..."` 是最自然的写法，后面的 flag 会被当成位置参数静默吃掉。
	// 所以自己拆一遍：命令前后的 flag 都收集起来，剩下的才是位置参数。
	flagArgs, cmd, pos := splitCommand(os.Args[1:])
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(2)
	}

	if cmd == "" {
		usage()
		os.Exit(2)
	}

	// 默认值不能有「覆盖」的权力：用户没显式传 -agent 时，provider 要沿用
	// meta 里记录的 agent（重启场景），而不是被 CLI 的默认值冲掉。
	agentExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "agent" {
			agentExplicit = true
		}
	})
	agentCmd := []string(nil)
	if agentExplicit {
		agentCmd = strings.Fields(o.agent)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p, err := sandbox.New(o.provider, sandbox.Options{
		Root:       o.root,
		ProxyBin:   resolveProxy(o.proxyBin),
		ProxyImage: o.template,
		CubeAPI:    o.cubeAPI,
		CubeToken:  o.cubeToken,
		Verbose:    o.verbose,
	})
	if err != nil {
		fatal(err)
	}

	id := ""
	if len(pos) > 0 {
		id = pos[0]
	}

	switch cmd {
	case "up":
		requireID(cmd, id)
		sb, err := p.Create(ctx, sandbox.CreateRequest{
			ID:        id,
			Template:  o.template,
			Workspace: o.workspace,
			Token:     o.token,
			Timeout:   o.timeout,
			Agent: sandbox.AgentSpec{
				Command:  agentCmd,
				ProxyBin: resolveProxy(o.proxyBin),
			},
		})
		if err != nil {
			fatal(err)
		}
		info, _ := sb.Info(ctx)
		fmt.Printf("\n\033[32m✓ 沙箱就绪\033[0m  id=%s  provider=%s  state=%s\n", info.ID, info.Provider, info.State)
		fmt.Printf("  endpoint  %s\n", info.Endpoint.URL)
		fmt.Printf("  token     %s\n", info.Endpoint.Token)
		if info.Workspace != "" {
			fmt.Printf("  workspace %s\n", info.Workspace)
		}
		if info.PID > 0 {
			fmt.Printf("  pid       %d\n", info.PID)
		}
		fmt.Printf("\n  连一下：\n    go run ./cmd/acp-smoke -url %s -token %s -skip-prompt\n\n",
			info.Endpoint.URL, info.Endpoint.Token)

	case "ls", "list":
		infos, err := p.List(ctx)
		if err != nil {
			fatal(err)
		}
		if len(infos) == 0 {
			fmt.Printf("（%s 下没有沙箱）\n", p.Name())
			return
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tENDPOINT\tWORKSPACE\tPID")
		for _, i := range infos {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", i.ID, i.State, i.Endpoint.URL, i.Workspace, i.PID)
		}
		tw.Flush()

	case "endpoint":
		requireID(cmd, id)
		sb, err := p.Acquire(ctx, id)
		if err != nil {
			fatal(err)
		}
		ep := sb.Endpoint()
		want := ""
		if len(pos) > 1 {
			want = pos[1]
		}
		if want == "url" {
			fmt.Println(ep.URL)
		} else if want == "token" {
			fmt.Println(ep.Token)
		} else {
			fmt.Printf("url=%s\ntoken=%s\n", ep.URL, ep.Token)
		}

	case "status":
		requireID(cmd, id)
		sb, err := p.Acquire(ctx, id)
		if err != nil {
			fatal(err)
		}
		info, _ := sb.Info(ctx)
		fmt.Printf("id=%s provider=%s state=%s workspace=%s\n", info.ID, info.Provider, info.State, info.Workspace)

	case "exec":
		requireID(cmd, id)
		rest := pos[1:]
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			fatal(errors.New("用法：astra-sandbox exec <id> -- <命令>"))
		}
		sb, err := p.Acquire(ctx, id)
		if err != nil {
			fatal(err)
		}
		res, err := sb.Exec(ctx, rest, sandbox.ExecOpts{Timeout: 60 * time.Second})
		if err != nil {
			fatal(err)
		}
		os.Stdout.Write(res.Stdout)
		os.Stderr.Write(res.Stderr)
		if res.ExitCode != 0 {
			os.Exit(res.ExitCode)
		}

	case "snapshot":
		requireID(cmd, id)
		sb, err := p.Acquire(ctx, id)
		if err != nil {
			fatal(err)
		}
		name := ""
		if len(pos) > 1 {
			name = pos[1]
		}
		path, err := sb.Snapshot(ctx, name)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("✓ 快照 %s\n", path)

	case "pause":
		requireID(cmd, id)
		sb, err := p.Acquire(ctx, id)
		if err != nil {
			fatal(err)
		}
		if err := sb.Pause(ctx); err != nil {
			fatal(err)
		}
		fmt.Printf("✓ 已暂停 %s\n", id)

	case "resume":
		requireID(cmd, id)
		sb, err := p.Acquire(ctx, id)
		if errors.Is(err, sandbox.ErrNotRunning) {
			fatal(errors.New("沙箱存在但已停；local provider 请直接 `up` 重建，它会在原目录上拉起"))
		}
		if err != nil {
			fatal(err)
		}
		if err := sb.Resume(ctx); err != nil {
			fatal(err)
		}
		fmt.Printf("✓ 已恢复 %s\n", id)

	case "rm", "kill":
		requireID(cmd, id)
		// 用 Destroy 而不是 Acquire+Kill：已停/已暂停的沙箱 Acquire 会返回
		// ErrNotRunning 拿不到句柄，那样清理就变成「打印成功但目录还在」。
		err := p.Destroy(ctx, id)
		if errors.Is(err, sandbox.ErrNotFound) {
			fmt.Printf("（%s 本来就不存在）\n", id)
			return
		}
		if err != nil {
			fatal(err)
		}
		fmt.Printf("✓ 已销毁 %s\n", id)

	case "doctor":
		doctor(ctx, p, o)

	default:
		usage()
		os.Exit(2)
	}
}

// doctor 一次性把「当前 provider 缺什么」列清楚，不要等到 up 才报错。
func doctor(ctx context.Context, p sandbox.Provider, o options) {
	fmt.Printf("provider : %s\n", p.Name())
	fmt.Printf("go       : %s\n", runtimeVersion())

	switch p.Name() {
	case "local":
		fmt.Printf("seatbelt : ")
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
			fmt.Println("可用（写限制生效）")
		} else {
			fmt.Println("缺失，降级为无强制隔离")
		}
		fmt.Printf("clonefile: ")
		if err := probeClonefile(); err == nil {
			fmt.Println("可用（APFS 快照毫秒级）")
		} else {
			fmt.Printf("不可用，快照会退化为普通复制（%v）\n", err)
		}
	case "applecontainer":
		if ac, ok := p.(interface {
			CheckReady(context.Context) error
		}); ok {
			if err := ac.CheckReady(ctx); err != nil {
				fmt.Printf("container: \033[31m未就绪\033[0m\n%v\n", err)
			} else {
				fmt.Println("container: 就绪")
			}
		}
	case "cube":
		fmt.Printf("cube api : %s\n", o.cubeAPI)
		if o.cubeAPI == "" {
			fmt.Println("  ⚠️  未配置 ASTRA_CUBE_API")
		} else {
			// 真探测：GET /sandboxes。200（有列表）或 404（控制面在、形状待核）
			// 都算「可达」；只有连不上才算不可用。
			// 形状本身由 sandbox 包的契约测试钉住；这里验证的是网络与凭据。
			client := &http.Client{Timeout: 5 * time.Second}
			req, _ := http.NewRequest(http.MethodGet,
				strings.TrimRight(o.cubeAPI, "/")+"/sandboxes", nil)
			if o.cubeToken != "" {
				req.Header.Set("Authorization", "Bearer "+o.cubeToken)
			}
			resp, err := client.Do(req)
			switch {
			case err != nil:
				fmt.Printf("  \033[31m不可达\033[0m: %v\n", err)
			case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusNotFound:
				fmt.Printf("  可达（HTTP %d）；形状由 sandbox 包契约测试覆盖\n", resp.StatusCode)
				resp.Body.Close()
			case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
				fmt.Printf("  \033[31m凭据被拒\033[0m（HTTP %d）\n", resp.StatusCode)
				resp.Body.Close()
			default:
				fmt.Printf("  异常响应 HTTP %d\n", resp.StatusCode)
				resp.Body.Close()
			}
		}
		fmt.Println("  ⚠️  cube provider 需 x86_64+KVM 宿主；Apple Silicon 上仅可达性/契约可验")
	}

	fmt.Printf("proxy    : ")
	if b := resolveProxy(o.proxyBin); b != "" {
		fmt.Println(b)
	} else {
		fmt.Println("\033[31m未找到 acp-proxy（先 make build）\033[0m")
	}
	fmt.Printf("agent    : %s\n", o.agent)
	if len(strings.Fields(o.agent)) > 0 {
		bin := strings.Fields(o.agent)[0]
		if _, err := exec.LookPath(bin); err != nil {
			if _, statErr := os.Stat(bin); statErr != nil {
				fmt.Printf("  ⚠️  %q 不在 PATH：%v\n", bin, err)
			}
		}
	}
}

func runtimeVersion() string {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "go version "))
}

// probeClonefile 探测 APFS 写时复制是否可用。
//
// 必须自建一个干净的小目录来试：直接拿 os.TempDir() 当源会撞上满地的 unix socket
// 与受限扩展属性，cp 报一堆 "Operation not permitted"，看起来像不支持克隆，
// 其实只是那个目录本身不能完整复制。
func probeClonefile() error {
	src, err := os.MkdirTemp("", "astra-clone-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	defer os.RemoveAll(src + "-dst")

	if err := os.WriteFile(filepath.Join(src, "probe.bin"), make([]byte, 1<<20), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-c", "-R", src, src+"-dst").CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	// 克隆出来的内容必须真的可读，否则「成功」没意义
	if _, err := os.Stat(filepath.Join(src+"-dst", "probe.bin")); err != nil {
		return err
	}
	return nil
}

// resolveProxy 找 acp-proxy：显式参数 > 环境变量 > PATH > 仓库 bin/。
// 多一层仓库 bin/ 是因为本地开发最常跑 `make build` 然后直接用 bin 里的产物。
func resolveProxy(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("ASTRA_PROXY_BIN"); env != "" {
		return env
	}
	if p, err := exec.LookPath("acp-proxy"); err == nil {
		return p
	}
	candidates := []string{"bin/acp-proxy", "./bin/acp-proxy"}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				return abs
			}
		}
	}
	return ""
}

// defaultAgent 推断 agent 命令。
//
// 查找顺序（前面的优先，每一条都是为了少让用户敲一遍 -agent）：
//
//	ASTRA_AGENT 环境变量 → PATH 里的 dsh → ~/.astra/agent-dsh（本机安装位）→ 仓库 dsh-trim
//
// 为什么要有 ~/.astra/agent-dsh 这一档：dsh 默认不全局安装，而装到 /tmp 里重启就没了。
// 约定一个固定位置，本地调试才能「一条 make dev 就跑起来」。
func defaultAgent() string {
	return strings.Join(sandbox.DefaultAgentCommand(), " ")
}

// splitCommand 把 argv 拆成「flag 参数」「子命令」「位置参数」三段。
//
// 为什么要自己拆：Go 的 flag 包遇到第一个非 flag 参数就停止解析，
// 于是 `astra-sandbox up dev -agent "node bin.js"` 里的 -agent 会被静默丢掉——
// 表现为「明明传了 agent 却还是用默认值」，排查起来很费时间。
//
// 规则：
//   - 命令前后的 flag 都收集（布尔 flag 不吞下一个 token，非布尔的吞）
//   - `--` 之后一律当位置参数，不再解析（exec 用来传子命令）
func splitCommand(argv []string) (flagArgs []string, cmd string, pos []string) {
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pos = append(pos, argv[i+1:]...)
			return
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			cmd = a
			i++
			break
		}
		tokens, consumed := takeFlag(argv, i)
		flagArgs = append(flagArgs, tokens...)
		i += consumed - 1
	}

	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pos = append(pos, argv[i+1:]...)
			return
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			tokens, consumed := takeFlag(argv, i)
			flagArgs = append(flagArgs, tokens...)
			i += consumed - 1
			continue
		}
		pos = append(pos, a)
	}
	return
}

// takeFlag 取出从 argv[i] 开始的一个 flag 及其可能的取值，返回 token 与被消耗的元素个数。
func takeFlag(argv []string, i int) ([]string, int) {
	a := argv[i]
	// -name=value 形式自成一体
	if strings.Contains(a, "=") {
		return []string{a}, 1
	}
	name := strings.TrimLeft(a, "-")
	f := flag.Lookup(name)
	if f == nil {
		// 未知 flag：只取自己，交给 flag.Parse 去报错
		return []string{a}, 1
	}
	// 布尔 flag 自带值，不吃下一个 token
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return []string{a}, 1
	}
	if i+1 < len(argv) {
		return []string{a, argv[i+1]}, 2
	}
	return []string{a}, 1
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func requireID(cmd, id string) {
	if id == "" {
		fatal(fmt.Errorf("用法：astra-sandbox %s <id>", cmd))
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "\n\033[31m✗ %v\033[0m\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `astra-sandbox —— 本地起沙箱，跑通 worker → acp-proxy → agent

用法：astra-sandbox [flag] <命令> [参数]

命令：
  up <id>             起沙箱（幂等：已存在则直接接管）
  ls                  列出沙箱
  endpoint <id> [url|token]   打印端点
  status <id>         状态
  exec <id> -- <cmd>  在沙箱里跑命令
  snapshot <id> [名]  冻结 workspace（local 用 APFS clonefile）
  pause <id>          暂停
  resume <id>         恢复
  rm <id>             销毁
  doctor              检查 provider 前置条件

常用 flag：
  -provider local|applecontainer|cube   （默认 local）
  -agent "dsh --profile acp"            agent 命令
  -workspace <dir>                      指定工作目录
  -root <dir>                           状态根目录（默认 ~/.astra/sandboxes）
`)
}
