package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Local 把「沙箱」实现为宿主机上的一个独立进程组 + 独立目录树。
//
// 能力与代价，说清楚免得误用：
//
//	有：目录隔离（每会话独立 workspace / HOME）、APFS clonefile 快照（毫秒级）、
//	    sandbox-exec 强制写限制（真实的内核级约束，不是约定）、进程独立可 detached
//	没有：内核隔离、网络策略、资源配额。**不可信代码不能用它跑** ——
//	    这是开发调试替身，生产必须走 cube/applecontainer。
//
// 目录布局（Root 默认 ~/.astra/sandboxes）：
//
//	<root>/<id>/workspace/   agent 的工作区
//	<root>/<id>/home/        HOME 与 DSH_HOME，会话持久化落这里，跨重启 resume 靠它
//	<root>/<id>/logs/        acp-proxy 的 stdout/stderr
//	<root>/<id>/meta.json    端口 / token / pid / 状态
//	<root>/<id>/snapshots/   workspace 快照（clonefile，几乎不占空间）
type Local struct {
	opts Options

	mu    sync.Mutex
	procs map[string]*exec.Cmd // 本进程内启动过的 acp-proxy，用于收尸与状态跟踪
}

// NewLocal 构造 local provider。
func NewLocal(opts Options) *Local {
	if opts.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		opts.Root = filepath.Join(home, ".astra", "sandboxes")
	}
	return &Local{opts: opts, procs: make(map[string]*exec.Cmd)}
}

func (l *Local) Name() string { return "local" }

// meta 落盘的沙箱元数据。Acquire 全靠它把沙箱「认回来」。
type meta struct {
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	Workspace string    `json:"workspace"`
	Home      string    `json:"home"`
	Agent     []string  `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
	State     State     `json:"state"`
	// Strict 记录创建时是否启用了 sandbox-exec，Resume 要沿用同一设置。
	Strict bool `json:"strict"`
	// NetworkPolicy 出网强制边界。必须落盘：Resume 只用
	// spawn(s.m, AgentSpec{Command: s.m.Agent}) 重启，不带 Env/新字段，
	// 只放内存会在 pause→resume 后静默失效，等于安全边界自己消失。
	NetworkPolicy *NetworkPolicy `json:"network_policy,omitempty"`
	// CLIBin 沙箱内 astra-cli 的绝对路径。必须落盘，理由同 NetworkPolicy：
	// Resume 用 spawn(s.m, AgentSpec{Command: s.m.Agent}) 重启、完全不带 Env，
	// 幂等重建（ensureClient 里传的是 m.cfg.Agent，不是带 Hints 的那份 spec）
	// 也可能不带 Env。只放内存会让 childEnv 的前置 PATH 注入在 resume / 重连后
	// 静默消失，沙箱里的 `astra-cli` 就又解析不到了。沿用落盘旧值是 fail-closed：
	// 漏传新值的请求不会把已注入的 CLI 路径抹掉。
	CLIBin string `json:"cli_bin,omitempty"`
}

// 只允许这些字符做 ID。这不是洁癖：ID 直接拼进文件路径，
// 一个 "../.." 就能让 RemoveAll 删到别处去。
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validateID(id string) error {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("sandbox: 非法 ID %q（只允许字母数字与 ._-，不能以 . 开头，不能含 ..）", id)
	}
	return nil
}

func (l *Local) dir(id string) string      { return filepath.Join(l.opts.Root, id) }
func (l *Local) metaPath(id string) string { return filepath.Join(l.dir(id), "meta.json") }

func (l *Local) readMeta(id string) (*meta, error) {
	b, err := os.ReadFile(l.metaPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("sandbox: 元数据损坏 %s: %w", l.metaPath(id), err)
	}
	return &m, nil
}

func (l *Local) writeMeta(m *meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// 0600：里面有 Bearer token
	return os.WriteFile(l.metaPath(m.ID), append(b, '\n'), 0o600)
}

// HostWorkspace 返回沙箱工作区的宿主路径（实现 WorkspaceLocator）。
//
// 不经过 Acquire：控制面「浏览文件」时沙箱可能已经 pause 甚至没在跑，
// 但目录还在、内容仍要能看。Acquire 在那种状态下会返回 ErrNotRunning，
// 用它做前置检查会让「看一眼上次的产出」变成一件做不到的事。
func (l *Local) HostWorkspace(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	m, err := l.readMeta(id)
	if err != nil {
		return "", err
	}
	if m.Workspace == "" {
		return "", fmt.Errorf("sandbox: %s 没有记录 workspace 路径", id)
	}
	return m.Workspace, nil
}

// Create 起沙箱。同 ID 且进程仍活着时直接返回现有句柄（幂等），
// 这样「重跑一次 make dev」不会因为端口被占而失败。
func (l *Local) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := validateID(req.ID); err != nil {
		return nil, err
	}

	root := l.dir(req.ID)
	ws := req.Workspace
	if ws == "" {
		ws = filepath.Join(root, "workspace")
	}
	hm := req.Agent.Home
	if hm == "" {
		hm = filepath.Join(root, "home")
	}
	for _, d := range []string{ws, hm, filepath.Join(root, "logs"), filepath.Join(root, "snapshots")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("sandbox: 建目录 %s: %w", d, err)
		}
	}

	// 已有沙箱目录（不管当前是 running 还是 paused/stopped）→ 在原端口上拉起。
	//
	// 为什么必须复用端口：生产 CubeSandbox 的 pause/resume 不改数据面地址，
	// 上层（worker）会缓存 endpoint。本地如果换了端口，就会出现「生产能恢复、本地恢复不了」
	// 的假性问题，调试时白白浪费时间。
	if old, err := l.readMeta(req.ID); err == nil {
		if old.State == StateRunning && pidAlive(old.PID) && healthy(old.Port) {
			return l.attachWithMeta(old), nil
		}
		// 端口若已被别人占走（极少见），才退回重新选端口
		if !portFree(old.Port) {
			old.Port = 0
		}
		if old.Port == 0 {
			p, err := l.pickPort()
			if err != nil {
				return nil, err
			}
			old.Port = p
		}
		if req.Token != "" {
			old.Token = req.Token
		}
		if len(req.Agent.Command) > 0 {
			old.Agent = req.Agent.Command
		}
		// 复用旧 meta 时：显式传了新网络策略就更新，否则沿用落盘的旧值。
		// 沿用旧值是刻意的 fail-closed——Resume / 二次 Create 漏传 policy 时，
		// 已启用的出网边界不会因为一次空请求而消失。spawn 始终以 old（meta）为准。
		if req.Agent.NetworkPolicy != nil {
			old.NetworkPolicy = req.Agent.NetworkPolicy
		}
		// CLI bin 同理：只在显式给了新值时更新，否则沿用落盘旧值。Resume /
		// ensureClient 的幂等重建不带 Env，靠这条保住 astra-cli 的 PATH 注入。
		if nb := req.Agent.Env["ASTRA_CLI_BIN"]; nb != "" {
			old.CLIBin = nb
		}
		old.State = StateRunning

		// 重启用合并后的 spec：调用方没传 agent 命令时沿用 meta 里记的，
		// 否则 spawn 会不加 -agent 而让 acp-proxy 退回内置默认值。
		spec := req.Agent
		if len(spec.Command) == 0 {
			spec.Command = old.Agent
		}
		if spec.Home == "" {
			spec.Home = old.Home
		}

		cmd, err := l.spawn(old, spec)
		if err != nil {
			return nil, err
		}
		old.PID = cmd.Process.Pid
		if err := waitHTTP(ctx, old.Port, 25*time.Second); err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("sandbox: 在原目录上重启 acp-proxy 失败（端口 %d）：%w（看 %s）",
				old.Port, err, filepath.Join(root, "logs", "proxy.log"))
		}
		if err := l.writeMeta(old); err != nil {
			_ = cmd.Process.Kill()
			return nil, err
		}
		return l.attachWithMeta(old), nil
	}

	port, err := l.pickPort()
	if err != nil {
		return nil, err
	}

	token := req.Token
	if token == "" {
		token = randomToken()
	}
	// 全新沙箱且调用方没指定 agent 时的兜底。
	// 放在 provider 而不是 CLI，是为了让「同一 ID 重启」永远沿用上次的 agent 命令。
	spec := req.Agent
	if len(spec.Command) == 0 {
		spec.Command = DefaultAgentCommand()
	}
	if spec.Home == "" {
		spec.Home = hm
	}

	m := &meta{
		ID:        req.ID,
		Port:      port,
		Token:     token,
		Workspace: ws,
		Home:      hm,
		// 存**解析后**的命令，不是 req 里的原始值。
		// 存原始值的话，调用方没传 -agent 时这里就是 nil，重启时 spawn 会不加 -agent，
		// acp-proxy 退回内置默认值 → 重启必然失败。
		Agent:     spec.Command,
		CreatedAt: time.Now(),
		State:     StateRunning,
		Strict:    l.strictFor(spec),
		// 出网强制边界随 meta 一起落盘，Resume 才能沿用（见 meta 字段注释）。
		NetworkPolicy: spec.NetworkPolicy,
		// 同理落盘 CLI bin：Resume 不带 Env，childEnv 从这里取回绝对路径。
		CLIBin: spec.Env["ASTRA_CLI_BIN"],
	}

	cmd, err := l.spawn(m, spec)
	if err != nil {
		return nil, err
	}
	m.PID = cmd.Process.Pid

	if err := waitHTTP(ctx, port, 25*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("sandbox: acp-proxy 没起来（端口 %d）：%w（看 %s）",
			port, err, filepath.Join(root, "logs", "proxy.log"))
	}

	if err := l.writeMeta(m); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return l.attachWithMeta(m), nil
}

// spawn 拉起 acp-proxy。strict 模式下外面套一层 sandbox-exec 做写限制。
func (l *Local) spawn(m *meta, agent AgentSpec) (*exec.Cmd, error) {
	proxy := l.opts.ProxyBin
	if proxy == "" {
		proxy = agent.ProxyBin
	}
	if proxy == "" {
		self, err := os.Executable()
		if err == nil {
			proxy = filepath.Join(filepath.Dir(self), "acp-proxy")
		}
	}

	argLine := strings.Join(agent.Command, " ")
	proxyArgs := []string{
		"-listen", fmt.Sprintf("127.0.0.1:%d", m.Port),
		"-cwd", m.Workspace,
		"-token", m.Token,
	}
	if argLine != "" {
		proxyArgs = append(proxyArgs, "-agent", argLine)
	}

	// m.Strict 在 Create 时已经算好：strictAvailable() && !spec.SelfSandbox。
	// agent 自带沙箱时这里**故意**不套 sandbox-exec：macOS 的 sandbox_apply 在
	// 已被沙箱化的进程里直接返回 Operation not permitted，沙箱不能嵌套——
	// 外层再套一层会让 agent 的每条命令都失败。强制边界只能有一层，
	// 由谁提供见 AgentSpec.SelfSandbox。命令构造见 spawnCommand。
	name, args := l.spawnCommand(m, proxy, proxyArgs)
	cmd := exec.Command(name, args...)
	cmd.Dir = m.Workspace
	cmd.Env = l.childEnv(m, agent)
	// Setsid：让 acp-proxy 脱离当前进程组。这样本进程退出后沙箱仍然活着，
	// 下次 Acquire 能认回来 —— 对应生产里「worker 重启不丢 agent」的语义。
	detach(cmd)

	logPath := filepath.Join(l.dir(m.ID), "logs", "proxy.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sandbox: 打开日志 %s: %w", logPath, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("sandbox: 启动 acp-proxy 失败: %w", err)
	}

	l.mu.Lock()
	l.procs[m.ID] = cmd
	l.mu.Unlock()

	// 收尸：进程退出后把状态写回 meta，Acquire 才知道该 Resume 还是重建。
	go func() {
		err := cmd.Wait()
		logFile.Close()
		l.mu.Lock()
		delete(l.procs, m.ID)
		l.mu.Unlock()

		if cur, rerr := l.readMeta(m.ID); rerr == nil && cur.PID == m.PID {
			cur.State = StateStopped
			if err != nil {
				cur.State = StateStopped
			}
			_ = l.writeMeta(cur)
		}
	}()

	return cmd, nil
}

// spawnCommand 决定要不要套 sandbox-exec、以及用哪个 profile 启动 acp-proxy。
//
// 这是「出网强制边界跟随 m.Strict」的唯一落点：m.Strict=false 时
// （ASTRA_NO_SEATBELT 非空 / sandbox-exec 不存在 / SelfSandbox=true）
// 根本不调用 profile，网络规则整体不生效——不需要给网络层单独做开关或绕过，
// 也就不会出现「写限制照常、出网限制单独被绕过」的缝隙。
//
// 抽成纯函数（无进程副作用）是为了能被单测直接锁住这条边界：
// 断言 strict 不可用时最终命令里既没有 -p 也没有 (deny network*)。
func (l *Local) spawnCommand(m *meta, proxy string, proxyArgs []string) (string, []string) {
	if m.Strict {
		return "sandbox-exec", append([]string{"-p", l.profile(m), proxy}, proxyArgs...)
	}
	return proxy, proxyArgs
}

// childEnv 构造 agent 的环境。核心是把 HOME 搬进沙箱目录：
// 这样 node/npm 的缓存、dsh 的配置与凭据全部落在沙箱内，互不污染。
//
// 另外把 ASTRA_CLI_BIN 所在目录前置到 PATH，让沙箱里的 `astra-cli ...` 真能解析到
// （设计文档 §2.2）：
//   - bin 由主控通过 AgentSpec.Env 注入，ASTRA_CLI_BIN 变量本身也保留在 env 里；
//   - Resume / 幂等重建不带 Env，退回 meta.CLIBin（见 meta 字段注释）；
//   - bin 不存在或没执行位时只记一句日志、不注入 PATH，**绝不让 spawn 失败**——
//     CLI 不可用应由 skillhub 标 unavailable，不是让整个沙箱起不来。
//
// 没有 CLI bin 时（含 Enabled=false / NetworkPolicy=nil 的全部既有场景），
// 这段逻辑整体不触发，返回的 env 与改动前逐字节一致。
func (l *Local) childEnv(m *meta, agent AgentSpec) []string {
	env := os.Environ()
	env = append(env,
		"HOME="+m.Home,
		"DSH_HOME="+m.Home,
		"TMPDIR="+filepath.Join(l.dir(m.ID), "tmp"),
		"ASTRA_SANDBOX_ID="+m.ID,
	)
	_ = os.MkdirAll(filepath.Join(l.dir(m.ID), "tmp"), 0o755)

	// 决定本次用哪个 bin：优先主控注入的 Env 值，否则用落盘的 meta.CLIBin。
	bin := agent.Env["ASTRA_CLI_BIN"]
	if bin == "" {
		bin = m.CLIBin
	}

	for k, v := range agent.Env {
		if k == "ASTRA_CLI_BIN" {
			continue // 归一成下面一条，避免重复 Create / Resume 留下重复键
		}
		env = append(env, k+"="+v)
	}
	if bin == "" {
		return env
	}
	// 无论 Env 里原本有没有，都保证 ASTRA_CLI_BIN 恰好出现一次：Resume 时
	// agent.Env 为空，这一步把主控注入过的变量补回来，skillhub 才读得到。
	env = append(env, "ASTRA_CLI_BIN="+bin)

	if !cliBinExecutable(bin) {
		log.Printf("sandbox: ASTRA_CLI_BIN %q 不存在或不可执行，跳过 PATH 注入（CLI 由 skillhub 标 unavailable，不影响沙箱启动）", bin)
		return env
	}
	return prependPath(env, filepath.Dir(bin))
}

// cliBinExecutable 判断 bin 是否是可直接执行的文件，只用于决定要不要注入 PATH。
// 结果刻意保守：拿不准就当不可用（记日志、不注入），但绝不会让 spawn 失败。
func cliBinExecutable(bin string) bool {
	fi, err := os.Stat(bin)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // Windows 没有 Unix 执行位，能 stat 到普通文件即可
	}
	return fi.Mode().Perm()&0o111 != 0
}

// prependPath 把 dir 前置到 env 里的 PATH，保留原 PATH（用 os.PathListSeparator 拼）。
// env 里可能有多条 PATH（os.Environ 一条 + agent.Env 覆盖一条），归并成一条：
// 取最后一条的值作原 PATH，前面拼 dir，避免 exec 收到重复键。
func prependPath(env []string, dir string) []string {
	orig := ""
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i >= 0 {
			key := kv[:i]
			// Unix 的 env 键大小写敏感，只认精确的 "PATH"，别误伤同名的 "path"；
			// Windows 的 env 键大小写不敏感，需要宽松匹配。
			if key == "PATH" || (runtime.GOOS == "windows" && strings.EqualFold(key, "PATH")) {
				orig = kv[i+1:]
				continue
			}
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+dir+string(os.PathListSeparator)+orig)
}

// profile 生成 sandbox-exec 的 Seatbelt 策略。
//
// 策略取向是「默认全允许，只掐写」。理由：agent 是 node 进程，要读系统库、
// 加载 node_modules、连 LLM，读全放开才不会莫名其妙起不来；而写限制恰好就是
// dsh 那三档 preset 里的 workspace-write 语义，能真正挡住「agent 改坏宿主文件」。
//
// 实测（macOS 26.2）：Node 的 V8 JIT 在该策略下正常工作，无需额外放行 executable memory。
func (l *Local) profile(m *meta) string {
	id := m.ID
	allow := []string{
		m.Workspace,
		m.Home,
		filepath.Join(l.dir(id), "tmp"),
		filepath.Join(l.dir(id), "logs"),
		filepath.Join(l.dir(id), "snapshots"),
		"/private/tmp",
		"/private/var/folders", // 系统临时目录，node/openssl 都会用
		"/dev",
	}
	var b strings.Builder
	b.WriteString("(version 1)(allow default)(deny file-write*)(allow file-write*")
	for _, p := range allow {
		b.WriteString(fmt.Sprintf(" (subpath %q)", p))
	}
	b.WriteString(")\n")
	// 出网强制边界：Enabled=false 时到此为止，与未启用本能力时逐字节相同
	// （原有 TestProfileCoversWorkspaceOnly 及逐字节兼容单测都依赖这一点）。
	if np := m.NetworkPolicy; np != nil && np.Enabled {
		b.WriteString(networkRules(np))
	}
	return b.String()
}

// sbplAddr 校验 "host:port" 并返回可用于 SBPL 的端口部分。
//
// 为什么生成时不照抄 host：macOS 26 的 Seatbelt ip/tcp filter 里 "network address"
// 的 host 只接受 "*" 或 "localhost"，写数字 IP 会直接编译失败：
//
//	sandbox-exec: host must be * or localhost in network address
//	sandbox-exec: port missing in network address: 127.0.0.1
//
// 这是在本机 macOS 26.2 用 /usr/bin/sandbox-exec 逐个 form 实测的，不是查文档猜的。
// 因此把 host 统一归一化为 localhost（networkRules 里拼 "localhost:<port>"），
// 即「只放行本机回环的该端口」——正好是「强制所有出网走本机代理」的语义。
// 用 "*" 会放行任意 IP 的该端口（外部主机也放行），过宽，不用。
//
// 校验本身也是 SBPL 注入防线：地址最终会被拼进 (remote ip "...")，
// 任何能闭合字符串 / form 的字符都拒绝，非法值由调用方忽略并记日志。
// 非回环的合法 IP 虽通过校验，归一化后也只放行回环端口（fail-closed，不会意外放开外网）。
func sbplAddr(addr string) (port string, ok bool) {
	// 白名单：合法地址只含这些字符。空格、引号、括号、分号、换行等一律拒绝。
	const safe = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.:[]*"
	for _, r := range addr {
		if !strings.ContainsRune(safe, r) {
			return "", false
		}
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	// IPv6 带方括号时 SplitHostPort 已去掉括号，这里再兜一层防御。
	bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	// host 必须是回环别名或合法数值 IP；不接受域名（Seatbelt 按 IP 匹配，
	// 域名无意义且会绕过代理的域名策略）。
	if bare != "localhost" && net.ParseIP(bare) == nil {
		return "", false
	}
	if p != "*" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	return p, true
}

// networkRules 生成「出网强制边界」的 SBPL 顶层 form，追加在写限制收口之后。
//
// SBPL 是 last-match-wins：deny 必须在前，具体 allow 在后；不同 operation 必须
// 拆成独立 form——写成 (allow network* (local ip ...)(remote ip ...)) 会因为
// local ip 匹配未绑定 outbound socket 而把外部出网重新放开（社区实测过的坑）。
//
// 真机（macOS 26.2）语法依据：
//   - (deny network*)                              编译通过
//   - (remote ip "localhost:17890")              编译通过；数字 IP 写法编译失败
//   - (remote ip "localhost:*") / (local ip ...) 编译通过；必须带 :port
//   - (allow network-outbound (remote unix-socket)) 编译通过（DNS / mDNSResponder）
func networkRules(np *NetworkPolicy) string {
	var b strings.Builder
	b.WriteString("(deny network*)\n")
	if port, ok := sbplAddr(np.ProxyAddr); ok {
		b.WriteString(fmt.Sprintf("(allow network-outbound (remote ip %q))\n", "localhost:"+port))
	} else {
		log.Printf("sandbox: 忽略非法 NetworkPolicy.ProxyAddr %q（仅接受 host:port，防 SBPL 注入）", np.ProxyAddr)
	}
	// DNS / mDNSResponder：域名解析走本机 unix-socket。不放行会 "Could not resolve host"，
	// agent 连「连接被拒」都得不到，等于把诊断信息也一起掐了。
	b.WriteString("(allow network-outbound (remote unix-socket))\n")
	// 控制面连 acp-proxy（inbound）+ acp-proxy 自己监听（bind），限回环。
	// 独立 operation、独立 form，不与 remote 拼在一起（见函数注释）。
	b.WriteString("(allow network-inbound (local ip \"localhost:*\"))\n")
	b.WriteString("(allow network-bind (local ip \"localhost:*\"))\n")
	for _, a := range np.ExtraAllow {
		port, ok := sbplAddr(a)
		if !ok {
			log.Printf("sandbox: 忽略非法 NetworkPolicy.ExtraAllow 项 %q（仅接受 host:port，防 SBPL 注入）", a)
			continue
		}
		b.WriteString(fmt.Sprintf("(allow network-outbound (remote ip %q))\n", "localhost:"+port))
	}
	return b.String()
}

// strictFor 决定这个 agent 要不要由平台包一层 sandbox-exec。
//
// 单独抽出来是为了能被测试直接锁住：这是「沙箱不能嵌套」那条约束的唯一落点，
// 写成内联表达式的话，将来谁改成 || 都不会有测试报警。
func (l *Local) strictFor(spec AgentSpec) bool {
	return l.strictAvailable() && !spec.SelfSandbox
}

// strictAvailable 检测 sandbox-exec 是否存在。缺失时降级为「无强制隔离」并告警，
// 而不是直接失败——本地开发不该因为系统差异起不来。
func (l *Local) strictAvailable() bool {
	if os.Getenv("ASTRA_NO_SEATBELT") != "" {
		return false
	}
	_, err := os.Stat("/usr/bin/sandbox-exec")
	return err == nil
}

func (l *Local) pickPort() (int, error) {
	used := map[int]bool{}
	l.mu.Lock()
	for id := range l.procs {
		if m, err := l.readMeta(id); err == nil {
			used[m.Port] = true
		}
	}
	l.mu.Unlock()

	// 先扫一遍磁盘上的 meta，避免和「另一个进程起的沙箱」撞端口
	if entries, err := os.ReadDir(l.opts.Root); err == nil {
		for _, e := range entries {
			if m, err := l.readMeta(e.Name()); err == nil && m.State == StateRunning {
				used[m.Port] = true
			}
		}
	}

	start := 41000 + rand.Intn(2000)
	for i := 0; i < 2000; i++ {
		p := 41000 + (start-41000+i)%2000
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, errors.New("sandbox: 41000-43000 没有空闲端口")
}

func healthy(port int) bool {
	client := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func randomToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// Acquire 接管已存在的沙箱（worker 重启 / failover）。
func (l *Local) Acquire(ctx context.Context, id string) (Sandbox, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	m, err := l.readMeta(id)
	if err != nil {
		return nil, err
	}
	if m.State == StateRunning && pidAlive(m.PID) && healthy(m.Port) {
		return l.attachWithMeta(m), nil
	}
	return nil, ErrNotRunning
}

// Destroy 销毁沙箱，不要求它活着。先杀进程组（若还在），再删目录。
func (l *Local) Destroy(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	m, err := l.readMeta(id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if m != nil && pidAlive(m.PID) {
		// 负 PID 杀整个进程组：dsh 是 acp-proxy 的子进程，单杀父会留孤儿
		killGroup(m.PID)
	}
	l.mu.Lock()
	delete(l.procs, id)
	l.mu.Unlock()
	if err := os.RemoveAll(l.dir(id)); err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	return nil
}

// List 枚举本 provider 下的沙箱。
func (l *Local) List(ctx context.Context) ([]Info, error) {
	entries, err := os.ReadDir(l.opts.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := l.readMeta(e.Name())
		if err != nil {
			continue
		}
		// 状态以实际进程为准，不信 meta（进程可能被外部杀掉）
		state := StateStopped
		if m.State == StateRunning && pidAlive(m.PID) && healthy(m.Port) {
			state = StateRunning
		}
		out = append(out, Info{
			ID:        m.ID,
			Provider:  "local",
			State:     state,
			Endpoint:  Endpoint{URL: fmt.Sprintf("ws://127.0.0.1:%d/acp", m.Port), Token: m.Token},
			Workspace: m.Workspace,
			PID:       m.PID,
			CreatedAt: m.CreatedAt,
			Detail:    "home=" + m.Home,
		})
	}
	return out, nil
}

func (l *Local) attachWithMeta(m *meta) *localSandbox {
	return &localSandbox{p: l, m: m}
}

// localSandbox 是一个已经起好的本地沙箱句柄。
type localSandbox struct {
	p *Local
	m *meta
}

func (s *localSandbox) ID() string { return s.m.ID }

func (s *localSandbox) Endpoint() Endpoint {
	return Endpoint{
		URL:   fmt.Sprintf("ws://127.0.0.1:%d/acp", s.m.Port),
		Token: s.m.Token,
	}
}

func (s *localSandbox) Info(ctx context.Context) (Info, error) {
	state := StateStopped
	if s.m.State == StateRunning && pidAlive(s.m.PID) && healthy(s.m.Port) {
		state = StateRunning
	}
	return Info{
		ID:        s.m.ID,
		Provider:  "local",
		State:     state,
		Endpoint:  s.Endpoint(),
		Workspace: s.m.Workspace,
		PID:       s.m.PID,
		CreatedAt: s.m.CreatedAt,
		Detail:    "home=" + s.m.Home,
	}, nil
}

// Exec 在沙箱环境里跑命令。注意：local 的 Exec **不套 sandbox-exec** ——
// 它是给平台自己用的（取产出物、跑预检），不是给 agent 用的。
// agent 的写限制在它自己的进程组上已由 spawn 时的 profile 保证。
func (s *localSandbox) Exec(ctx context.Context, cmdline []string, opts ExecOpts) (ExecResult, error) {
	if len(cmdline) == 0 {
		return ExecResult{}, errors.New("sandbox: 空命令")
	}
	cctx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cctx, cmdline[0], cmdline[1:]...)
	cmd.Dir = s.m.Workspace
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	cmd.Env = append(os.Environ(), "HOME="+s.m.Home, "DSH_HOME="+s.m.Home)
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(opts.Stdin) > 0 {
		cmd.Stdin = strings.NewReader(string(opts.Stdin))
	}

	res := ExecResult{}
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	res.Stdout = out
	res.Stderr = []byte(errb.String())
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil // 非零退出不是「调用失败」，交给调用方判断
		}
		return res, err
	}
	return res, nil
}

// Snapshot 用 APFS clonefile 冻结 workspace。
//
// 这是 local provider 最占便宜的地方：`cp -c` 是写时复制，50MB 目录实测 10ms、
// 几乎不占额外空间。所以本地可以随手打快照当模板用，不需要镜像构建那一整套。
func (s *localSandbox) Snapshot(ctx context.Context, name string) (string, error) {
	if name == "" {
		name = fmt.Sprintf("snap-%d", time.Now().Unix())
	}
	if err := validateID(name); err != nil {
		return "", err
	}
	dst := filepath.Join(s.p.dir(s.m.ID), "snapshots", name)
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}

	// -c 走 clonefile（APFS 写时复制）；源目录末尾的 / 不能丢，否则会把目录本身嵌套复制。
	//
	// 但 -c 是 macOS 独占：GNU cp 没有这个选项，会直接报 "invalid option -- 'c'"。
	// 原实现只在输出里含 clonefile / not supported 时才退化，Linux 上这句话两头都不沾，
	// 于是把「参数不支持」当成了「快照失败」——CI 在 ubuntu 上红了十几次就是这个。
	// 现在按平台分：darwin 先试写时复制，其余平台（以及非 APFS 卷）走普通复制。
	var first string
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "cp", "-c", "-R", s.m.Workspace, dst).CombinedOutput()
		if err == nil {
			return dst, nil
		}
		first = strings.TrimSpace(string(out))
	}
	// 普通复制：语义与 clonefile 一致，只是慢、占实际空间
	if out, err := exec.CommandContext(ctx, "cp", "-R", s.m.Workspace, dst).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if first != "" {
			msg = first + "；退化为普通复制后仍失败: " + msg
		}
		return "", fmt.Errorf("sandbox: 快照失败: %s", msg)
	}
	return dst, nil
}

// Pause 停掉进程但保留全部目录。
//
// 与 CubeSandbox 的 pause 有实质差异：那边是内存快照（进程树冻结、恢复后内存原样），
// 这边只是把进程杀掉。所以本地 pause 之后 agent 的**内存态会丢**，只能靠
// DSH_HOME 里的会话持久化 + session/resume 恢复。跨 provider 的契约只保证
// 「workspace 与 home 不丢」，不保证内存态。
func (s *localSandbox) Pause(ctx context.Context) error {
	if !pidAlive(s.m.PID) {
		s.m.State = StatePaused
		return s.p.writeMeta(s.m)
	}
	if err := terminate(s.m.PID); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(s.m.PID) {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}
	if pidAlive(s.m.PID) {
		killForce(s.m.PID)
	}
	s.m.State = StatePaused
	return s.p.writeMeta(s.m)
}

// Resume 在保留的目录上重新拉起 agent。会话靠 DSH_HOME + session/resume 恢复。
func (s *localSandbox) Resume(ctx context.Context) error {
	if pidAlive(s.m.PID) && healthy(s.m.Port) {
		s.m.State = StateRunning
		return s.p.writeMeta(s.m)
	}

	cmd, err := s.p.spawn(s.m, AgentSpec{Command: s.m.Agent})
	if err != nil {
		return err
	}
	s.m.PID = cmd.Process.Pid
	s.m.State = StateRunning
	if err := waitHTTP(ctx, s.m.Port, 25*time.Second); err != nil {
		return fmt.Errorf("sandbox: resume 后 acp-proxy 未就绪: %w", err)
	}
	return s.p.writeMeta(s.m)
}

// Close 只释放句柄，沙箱继续活着（对应「worker 重启不丢 agent」）。
func (s *localSandbox) Close() error { return nil }

// Kill 销毁：杀进程 + 删目录。不可恢复。
func (s *localSandbox) Kill(ctx context.Context) error {
	if pidAlive(s.m.PID) {
		killGroup(s.m.PID) // 负 PID 杀整个进程组（dsh 会被一起带走）
	}
	s.p.mu.Lock()
	delete(s.p.procs, s.m.ID)
	s.p.mu.Unlock()
	return os.RemoveAll(s.p.dir(s.m.ID))
}

// 让编译器提醒我们接口变更时同步实现。
var _ Provider = (*Local)(nil)
var _ Sandbox = (*localSandbox)(nil)
