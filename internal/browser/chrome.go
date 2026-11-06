package browser

// Chrome 的定位、启动与 DevTools 端点解析。
//
// 这里只做「把进程拉起来并拿到 CDP 地址」这一件事：起一个新进程、读它的 stderr、
// 找到那一行 DevTools 地址。协议层（CDP 本身）不在平台侧实现——agent 在沙箱里
// 用 playwright/chromedp 连过去。

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// devToolsPrefix 是 Chrome 在 stderr 上打的那行前缀，形如：
	//   DevTools listening on ws://127.0.0.1:53412/devtools/browser/2f1c...
	devToolsPrefix = "DevTools listening on "
)

// locateChrome 按 **显式配置 → CHROME_PATH → 平台默认位置** 的顺序找 Chrome。
// 全都找不到时返回包着 ErrDisabled 的错误，由 app 层降级（平台照常启动）。
func locateChrome(explicit string) (string, error) {
	var tried []string
	seen := map[string]bool{}
	for _, cand := range chromeCandidates(explicit) {
		if cand == "" || seen[cand] {
			continue
		}
		seen[cand] = true
		if p, ok := resolveExec(cand); ok {
			return p, nil
		}
		tried = append(tried, cand)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("%w：未配置 CHROME_PATH", ErrDisabled)
	}
	return "", fmt.Errorf("%w：已尝试 %s", ErrDisabled, strings.Join(tried, "、"))
}

// chromeCandidates 给出待尝试的候选列表（按优先级排列，未去重）。
func chromeCandidates(explicit string) []string {
	out := make([]string, 0, 8)
	if v := strings.TrimSpace(explicit); v != "" {
		out = append(out, v)
	}
	if v := strings.TrimSpace(os.Getenv("CHROME_PATH")); v != "" {
		out = append(out, v)
	}
	return append(out, platformChromeCandidates()...)
}

func platformChromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		cands := []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
		// 非管理员安装的 Chrome 装在 ~/Applications 下，顺手带上。
		if home, err := os.UserHomeDir(); err == nil {
			cands = append(cands,
				filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
				filepath.Join(home, "Applications/Chromium.app/Contents/MacOS/Chromium"),
			)
		}
		return cands
	case "windows":
		var cands []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			cands = append(cands,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Chromium", "Application", "chrome.exe"),
			)
		}
		return cands
	default: // linux / 其他类 unix
		return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	}
}

// resolveExec 判断候选是否可用：带路径分隔符的按文件检查，裸名字走 PATH。
func resolveExec(cand string) (string, bool) {
	if strings.ContainsAny(cand, `/\`) {
		st, err := os.Stat(cand)
		if err != nil || st.IsDir() {
			return "", false
		}
		// Windows 没有可执行位，不做这个检查。
		if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
			return "", false
		}
		return cand, true
	}
	p, err := exec.LookPath(cand)
	if err != nil {
		return "", false
	}
	return p, true
}

// chromeArgs 拼启动参数。
//
// 关键三条：
//   - remote-debugging-port=0 让内核挑端口，多实例不会撞车；
//   - user-data-dir 每个实例独立，cookie/localStorage 天然隔离——同账号多用户的
//     登录态不能互通（这是 Grok Bot 被点名的坑）；
//   - 一堆 disable-* 是为了让 headless 在容器/无桌面环境里稳一点，也避免
//     Chrome 自己去连更新服务器（那部分流量不受白名单约束）。
func chromeArgs(dir string, headless bool, resolverRules string) []string {
	args := make([]string, 0, 14)
	if headless {
		args = append(args, "--headless=new")
	}
	args = append(args,
		"--remote-debugging-port=0",
		"--user-data-dir="+dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--disable-sync",
	)
	if runtime.GOOS == "linux" {
		// Linux 容器 / CI runner 里 Chrome 的 setuid sandbox 基本起不来
		// （user namespace 常被 seccomp 挡住），不加这两条就永远等不到 DevTools 行，
		// 症状是「10s 超时」而 stderr 里其实写着 Failed to move to new namespace。
		// 安全边界由平台层提供（cube microVM / 沙箱），Chrome 自带沙箱是第二层；
		// 何况每个实例都是独立 profile + 用完即删，本来就不该拿它当隔离边界。
		args = append(args, "--no-sandbox", "--disable-setuid-sandbox")
	}
	if resolverRules != "" {
		args = append(args, "--host-resolver-rules="+resolverRules)
	}
	// 给一个初始页面，agent 连上就有 target 可用（否则 List 里 Pages 是 0）。
	args = append(args, "about:blank")
	return args
}

// hostResolverRules 把不在白名单里的域名解析到 0.0.0.0，从 DNS 层面掐断导航。
//
// 为什么不用 CDP 的 Target.setDiscoverTargets + Page.frameNavigated 去拦：
// 那套要自己维护 target 附件、处理子 frame、还要跟 JS 跳转/重定向抢时间窗，
// 一期落地成本高且容易漏。--host-resolver-rules 是 Chrome 官方支持的参数，
// 在 DNS 解析这一层生效，成本几乎为零。
//
// **局限（很重要，别当成沙箱）**：
//  1. 只拦「需要域名解析」的请求。直接写 IP 的地址（http://1.2.3.4/）绕过解析，
//     仍然可达；--proxy-server 那套才能覆盖。
//  2. 对**已经建立的连接**无效：白名单是启动时快照，页面里长连接、WebSocket、
//     已缓存的 keep-alive 都不受影响。
//  3. 不拦下载/上传的目标地址里的 IP 直连，也不拦通过外部 DNS-over-HTTPS 拿到
//     IP 后再直连的路径（浏览器内的 DoH 仍会走系统解析器，但 IP 直连绕不过第 1 条）。
//  4. 子域名放行靠 `*.host` 通配，规则引擎不支持时会被一并拦掉——方向是 fail-closed，
//     不会误放。
//
// 所以它的定位是「减少误触」，真正的出网管控仍应由 egress 代理承担（见包注释）。
// 另外 0.0.0.0 在部分协议栈上等价于本机回环，被拦域名如果恰好撞上本机同端口
// 服务仍可能连通——这也是它不能当沙箱用的原因之一。
func hostResolverRules(allowed []string) (string, error) {
	hosts := normalizeHosts(allowed)
	if len(hosts) == 0 {
		return "", nil // 白名单为空 = 不限制（仅本地调试）
	}
	// 规则按顺序匹配，第一条命中的生效。EXCLUDE 必须排在 MAP * 前面，
	// 否则 MAP * 先把所有域名都吃掉了。
	// localhost / 回环一定要放行：agent 经常要访问沙箱里自己起的 dev server。
	rules := make([]string, 0, len(hosts)*2+5)
	for _, h := range append([]string{"localhost", "127.0.0.1", "::1"}, hosts...) {
		rules = append(rules, "EXCLUDE "+h)
		// 通配子域名；IP 字面量没有「子域名」这回事，加了只是噪音。
		if net.ParseIP(h) == nil {
			rules = append(rules, "EXCLUDE *."+h)
		}
	}
	rules = append(rules, "MAP * 0.0.0.0")
	return strings.Join(rules, ", "), nil
}

// normalizeHosts 清洗白名单：容忍带 scheme/端口/路径的写法，丢弃非法项。
// 丢弃是安全的（拦得更狠），不是 fail-open。
func normalizeHosts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		h, ok := normalizeHost(raw)
		if !ok || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func normalizeHost(raw string) (string, bool) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" {
		return "", false
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "*.")
	if h == "" {
		return "", false
	}
	// 只允许域名/IPv6 会用到的字符。空格、逗号、引号会把规则串搞坏，必须拒。
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return "", false
		}
	}
	if strings.HasPrefix(h, ".") || strings.HasPrefix(h, "-") {
		return "", false
	}
	return h, true
}

// parseDevToolsLine 从 Chrome 的一行 stderr 里抠出 CDP ws 地址。
// 只认 ws/wss + 带端口 + 路径里有 /devtools/browser/ 的地址，别的一律当噪声
// （Chrome 还爱打 "[ERROR:...]"、GPU 告警、扩展报错之类）。
func parseDevToolsLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	i := strings.Index(line, devToolsPrefix)
	if i < 0 {
		return "", false
	}
	raw := strings.TrimSpace(line[i+len(devToolsPrefix):])
	if j := strings.IndexAny(raw, " \t"); j >= 0 {
		raw = raw[:j]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", false
	}
	if u.Host == "" || u.Port() == "" {
		return "", false
	}
	if !strings.Contains(u.Path, "/devtools/browser/") {
		return "", false
	}
	return u.String(), true
}

// httpURLFromWS 由 ws 地址推出 HTTP 端点（/json/version、/json/list 都在这儿）。
func httpURLFromWS(ws string) string {
	u, err := url.Parse(ws)
	if err != nil || u.Host == "" {
		return ""
	}
	return "http://" + u.Host
}

// stderrTail 留下 Chrome stderr 的最后几行。
//
// 之前只挑 DevTools 那一行、其余全丢，于是 CI 上 Chrome 因为沙箱 / 缺动态库
// 起不来时，错误信息只有「10s 没等到 DevTools 端点」，真实原因一个字都看不到。
// 启动失败时把它拼进 error，下次红能直接定位，不用去翻需要 admin 权限的 job log。
type stderrTail struct {
	mu    sync.Mutex
	lines []string
}

func (t *stderrTail) add(line string) {
	if line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

// drainDevTools 逐行读 stderr，找到 DevTools 行后继续把管道读干净。
// 必须一直读到 EOF：中途不读了，Chrome 往 stderr 写满 64KB 就会阻塞。
func drainDevTools(r io.Reader, out chan<- string, tail *stderrTail) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	found := false
	for sc.Scan() {
		line := sc.Text()
		if tail != nil {
			tail.add(line)
		}
		if found {
			continue
		}
		if ws, ok := parseDevToolsLine(line); ok {
			found = true
			select {
			case out <- ws:
			default:
			}
		}
	}
}

// startChrome 起一个 Chrome 并等它报出 DevTools 地址。
// 返回的实例已经 Start 成功，done 在 cmd.Wait 返回后关闭。
//
// 任何一条失败路径都自己负责收拾（kill 进程 + 删目录），不把半启动的进程
// 交回上层——启动超时/被取消后还能留一堆 Chrome 在跑是最难排查的那类 bug。
func startChrome(ctx context.Context, bin, dir string, removeDir, headless bool, rules string) (*instance, error) {
	args := chromeArgs(dir, headless, rules)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir // 别让 Chrome 在平台进程的 cwd 里拉屎

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("browser: 接管 Chrome stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("browser: 启动 Chrome 失败: %w", err)
	}

	inst := &instance{dir: dir, removeDir: removeDir, cmd: cmd, done: make(chan struct{})}
	// 收回子进程必须在独立 goroutine 里做：不 Wait 就是僵尸，同步 Wait 又会卡住
	// 启动流程（Chrome 要活很久）。waitErr 在 close(done) 之前写，读侧靠 channel
	// 关闭建立 happens-before，不用额外加锁。
	go func() {
		inst.waitErr = cmd.Wait()
		close(inst.done)
	}()

	lines := make(chan string, 1)
	tail := &stderrTail{}
	go drainDevTools(stderr, lines, tail)

	abort := func(err error) (*instance, error) {
		killProcess(inst)
		inst.releaseDir()
		return nil, err
	}

	timer := time.NewTimer(launchTimeout)
	defer timer.Stop()
	for {
		select {
		case ws, ok := <-lines:
			if !ok {
				lines = nil // 管道关了但没找到地址，转去等进程退出/超时
				continue
			}
			httpURL := httpURLFromWS(ws)
			if httpURL == "" {
				return abort(fmt.Errorf("browser: DevTools 地址不可用: %q", ws))
			}
			inst.cdpURL, inst.httpURL = ws, httpURL
			return inst, nil
		case <-inst.done:
			killProcess(inst) // 进程已经退了，这里只是确保子进程也被收走
			inst.releaseDir()
			return nil, fmt.Errorf("browser: Chrome 启动后立即退出: %v；stderr: %s", inst.waitErr, tail.String())
		case <-ctx.Done():
			return abort(fmt.Errorf("browser: 启动被取消: %w", ctx.Err()))
		case <-timer.C:
			return abort(fmt.Errorf("browser: %s 内没等到 DevTools 端点；Chrome stderr: %s", launchTimeout, tail.String()))
		}
	}
}

// profileDir 决定 --user-data-dir。
//
// 默认每次新建临时目录，Close 时删掉：cookie 用完即焚，是最安全的默认。
// req.Profile 非空时改为按 tenant+user+profile 派生的持久目录（便利优先：
// 登录一次长期有效），此时 Close 不删目录——隔离仍然在用户粒度上。
func profileDir(req LaunchRequest) (dir string, remove bool, err error) {
	if strings.TrimSpace(req.Profile) == "" {
		d, err := os.MkdirTemp("", profileMarker)
		if err != nil {
			return "", false, fmt.Errorf("browser: 建临时 profile 目录失败: %w", err)
		}
		return d, true, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(req.TenantID + "\x00" + req.UserID + "\x00" + req.Profile))
	d := filepath.Join(base, "astra-agent", "browser-profiles", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", false, fmt.Errorf("browser: 建持久 profile 目录失败: %w", err)
	}
	return d, false, nil
}

// profileMarker 是临时目录前缀，测试用它到 ps 里找残留进程。
const profileMarker = "astra-browser-"
