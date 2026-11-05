// astra-desktop 是 astra-agent 的 macOS 桌面端。
//
// 形态：一个原生窗口（WKWebView）+ 同进程内嵌的完整控制面（internal/app）。
// 与命令行入口共用同一套装配，差别只在配置来源（config.json 而不是 flag）、
// 端口（内核分配而不是固定）、生命周期（关窗最小化到 Dock，Cmd+Q 才退出）。
//
// 凭据与数据的位置遵循 macOS 惯例：
//
//	数据/配置  ~/Library/Application Support/Astra/
//	日志       ~/Library/Logs/Astra/
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/WangYulong666/astra-agent/internal/app"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// version 由 build-app.sh 注入（-ldflags -X main.version=...），未注入时为 dev。
var version = "dev"

// desktopConfig 是用户在配置文件里能改的全部内容。字段刻意少——
// 桌面端的目标是「装上就能用」，不是把 flag 搬进 JSON。
type desktopConfig struct {
	// Agent shell / dsh。dsh 需要 LLMKey。
	Agent string `json:"agent,omitempty"`
	// LLMKey 上游模型 key。写进配置文件是本地单机 App 的务实取舍：
	// 文件权限 0600，且真 key 只进控制面内存、不进沙箱（虚拟 key 隔离）。
	LLMKey string `json:"llmKey,omitempty"`
	// LLMBaseURL 上游地址，空用 DeepSeek 官方。
	LLMBaseURL string `json:"llmBaseUrl,omitempty"`
	// Policy 权限预设，空 = ask。
	Policy string `json:"policy,omitempty"`
	// DataDir 数据目录，空用默认。
	DataDir string `json:"dataDir,omitempty"`
}

// current 是进程级单例：菜单回调与退出路径要拿到服务引用。
var current struct {
	srv     *app.Server
	cfg     desktopConfig
	baseURL string
	// appCtx 是 wails 生命周期上下文（OnStartup 注入），菜单回调里
	// 调 runtime.MessageDialog 等运行时能力都要用它。
	appCtx context.Context
}

func main() {
	lg, logPath := setupLogging()
	lg.Printf("Astra 桌面端启动 pid=%d log=%s", os.Getpid(), logPath)

	if err := acquireSingleInstance(); err != nil {
		lg.Printf("已有实例在运行: %v", err)
		notify("Astra 已在运行", "从 Dock 或菜单打开即可。")
		return
	}

	cfg := loadConfig(lg)
	current.cfg = cfg

	// dsh 可用性探针：配置要 dsh 而机器上没有时，起会话会莫名失败。
	// 与其让用户撞墙，启动时就降级为 shell 并把原因写清楚。
	if cfg.agent() == "dsh" {
		path, fromBundle, ok := dshStatus(dshBinHome(), dshBinBundle())
		switch {
		case fromBundle:
			// DefaultAgentCommand 只认 ~/.astra/agent-dsh（ASTRA_AGENT > PATH > home）。
			// bundle 里有而 home 没有 → 用它已有的 ASTRA_AGENT 钩子重定向，不改 internal。
			cmd := fmt.Sprintf("node %s --profile acp", path)
			os.Setenv("ASTRA_AGENT", cmd)
			lg.Printf("使用 bundle 内 dsh: %s（ASTRA_AGENT=%s）", path, cmd)
		case !ok:
			lg.Printf("⚠ 配置为 dsh 但未找到 dsh（检查过 %s 与 bundle 内 Resources/agent-dsh），本次降级为 shell 模式", dshBinHome())
			notify("dsh 未安装，已用 shell 模式", "模型不会思考。安装 dsh 后可重试 dsh 模式。")
			cfg.Agent = "shell"
			current.cfg = cfg
		default:
			lg.Printf("dsh 可用: %s", path)
		}
	}

	// 控制面与窗口同进程。装配失败（key 错、目录不可写）直接退出并写日志，
	// 用户侧表现为「点开没反应」——所以 fatal 前先发一条系统通知。
	srv, addr, err := startServer(cfg, lg)
	if err != nil {
		notify("Astra 启动失败", "详情见日志："+logPath)
		fatal(lg, err)
	}
	current.srv = srv
	current.baseURL = "http://" + addr
	lg.Printf("控制面地址 %s", current.baseURL)

	runWindow(lg)
}

// startServer 装配并启动内嵌控制面，返回服务与真实监听地址。
func startServer(cfg desktopConfig, lg *log.Logger) (*app.Server, string, error) {
	// 虚拟 key 要指向 agent 可达的网关地址，而端口在 Start 才确定；
	// 凭据签发器却在 New 时装配。先绑一个临时 listener 把端口定下来。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	appCfg := app.Config{
		Addr:         fmt.Sprintf("127.0.0.1:%d", port),
		DataDir:      cfg.dataDir(),
		Provider:     "local",
		Agent:        cfg.agent(),
		AgentEnv:     map[string]string{"DEEPSEEK_API_KEY": cfg.LLMKey},
		Policy:       cfg.policy(),
		StoreKind:    "sqlite",
		RequireAuth:  true,
		Bootstrap:    false, // 首启走 Web 向导：账号密码和 LLM key 都由用户自己填
		LLMGateway:   cfg.LLMKey != "",
		LLMUpstream:  cfg.llmBaseURL(),
		LLMKey:       cfg.LLMKey,
		LLMProvider:  "deepseek",
		LLMPublicURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		JWTSecret:    loadOrCreateSecret(cfg.dataDir(), lg),
		Logger:       lg,
	}
	srv, err := app.New(appCfg)
	if err != nil {
		return nil, "", err
	}

	// 审批请求 → 系统通知。这是桌面端存在的核心理由之一：
	// agent 卡在等人时，人可能正在干别的，不通知等于死锁。
	srv.Hub().OnGlobal(func(ev store.Event) {
		if ev.Type != store.EventApprovalReq {
			return
		}
		notify("Agent 请求审批", "有一个操作等你决定，打开 Astra 处理。")
	})

	addr, err := srv.Start()
	if err != nil {
		return nil, "", err
	}
	return srv, addr, nil
}

// ── 窗口 ────────────────────────────────────────────────────────────────

func runWindow(lg *log.Logger) {
	u, err := url.Parse(current.baseURL)
	if err != nil {
		fatal(lg, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1 // SSE 逐条透传，缓冲会把事件流变成「一次性泼出」
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		lg.Printf("proxy %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}

	err = wails.Run(&options.App{
		Title:     "Astra",
		Width:     1360,
		Height:    900,
		MinWidth:  960,
		MinHeight: 600,
		// 关窗只藏窗口不退进程：agent 可能还在沙箱里跑任务，
		// 「点了红点号就把任务杀了」是桌面 app 最招人恨的行为之一。
		HideWindowOnClose: true,
		BackgroundColour:  options.NewRGB(18, 20, 24),
		AssetServer:       &assetserver.Options{Handler: proxy},
		Menu:              buildMenu(lg),
		OnStartup: func(ctx context.Context) {
			current.appCtx = ctx
		},
		OnBeforeClose: func(ctx context.Context) bool {
			return false // 配合 HideWindowOnClose：永远只藏不关
		},
		OnShutdown: shutdown,
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title:   "Astra",
				Message: fmt.Sprintf("Agent 工作台 — 沙箱里干活的 AI 助手\n版本 %s", version),
			},
		},
	})
	if err != nil {
		fatal(lg, err)
	}
}

func shutdown(ctx context.Context) {
	if current.srv == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = current.srv.Shutdown(sctx)
	_ = current.srv.Close()
}

func buildMenu(lg *log.Logger) *menu.Menu {
	openPath := func(label, path string) *menu.MenuItem {
		return &menu.MenuItem{Label: label, Click: func(_ *menu.CallbackData) {
			if err := exec.Command("open", path).Start(); err != nil {
				lg.Printf("打开 %s 失败: %v", path, err)
			}
		}}
	}
	m := menu.NewMenu()
	m.Append(openPath("打开数据目录", current.cfg.dataDir()))
	m.Append(openPath("打开日志", logFilePath))
	m.Append(menu.Separator())

	// Agent 模式切换：写回 config.json，重启后由启动探针裁决是否可用。
	// 菜单只负责「表达意图」，可用性判断集中在一处（dshStatus），避免两处逻辑漂移。
	agentSub := m.AddSubmenu("Agent 模式")
	cur := current.cfg.agent()
	for _, mode := range []string{"shell", "dsh"} {
		mode := mode
		agentSub.Append(&menu.MenuItem{
			Label:   mode,
			Type:    menu.CheckboxType, // v2.10 支持 Checked，当前项打勾
			Checked: cur == mode,
			Click: func(_ *menu.CallbackData) {
				if mode == cur {
					return
				}
				if err := setAgentInConfig(mode); err != nil {
					lg.Printf("切换 agent 模式到 %s 失败: %v", mode, err)
					notify("切换失败", "无法写 config.json，详情见日志。")
					return
				}
				lg.Printf("agent 模式将切换为 %s（已写 config.json）", mode)
				if current.appCtx != nil {
					_, _ = wruntime.MessageDialog(current.appCtx, wruntime.MessageDialogOptions{
						Type:    wruntime.InfoDialog,
						Title:   "Astra",
						Message: "已切换为 " + mode + " 模式，重启 Astra 生效。",
					})
				} else {
					notify("已切换为 "+mode+" 模式", "重启 Astra 生效。")
				}
			},
		})
	}
	m.Append(&menu.MenuItem{Label: "备份数据", Click: func(_ *menu.CallbackData) {
		// 打包可能要几秒，放后台跑，别卡住菜单主线程。
		go func() {
			dst, err := backupData(current.cfg.dataDir())
			if err != nil {
				lg.Printf("备份数据失败: %v", err)
				notify("备份失败", "详情见日志。")
				return
			}
			notify("备份完成", dst)
		}()
	}})
	m.Append(menu.Separator())
	// 退出必须显式提供：红点被拦掉了，Cmd+Q 走这里。
	m.Append(&menu.MenuItem{Label: "退出 Astra", Accelerator: keys.CmdOrCtrl("q"),
		Click: func(_ *menu.CallbackData) {
			shutdown(context.Background())
			os.Exit(0)
		}})
	return m
}

// ── 配置 / 目录 / 单实例 ─────────────────────────────────────────────────

func appSupportDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Astra")
}

func (c desktopConfig) dataDir() string {
	if c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Join(appSupportDir(), "data")
}

func (c desktopConfig) agent() string {
	if c.Agent == "shell" || c.Agent == "dsh" {
		return c.Agent
	}
	return "shell" // 默认不带模型也能跑（shell agent），首启即可验证安装
}

func (c desktopConfig) policy() string {
	switch c.Policy {
	case "ask", "workspace-write", "allow-all", "deny-all":
		return c.Policy
	}
	return "ask"
}

func (c desktopConfig) llmBaseURL() string {
	if c.LLMBaseURL != "" {
		return c.LLMBaseURL
	}
	return "https://api.deepseek.com"
}

// setAgentInConfig 把 agent 字段写回 config.json。读-改-写而不是整份重造：
// 文件里可能有我们没解析的额外字段（用户手改的注释性内容之类），不该被抹掉。
func setAgentInConfig(agent string) error {
	p := filepath.Join(appSupportDir(), "config.json")
	var c desktopConfig
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	c.Agent = agent
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ── dsh 可用性探针 ──────────────────────────────────────────────────────
//
// 检测逻辑与 sandbox.DefaultAgentCommand 的查找顺序对齐（PATH > ~/.astra），
// 再加上 bundle Resources 这一档。函数接受路径参数而非绑死真实路径，便于单测。

// dshBinHome 返回 ~/.astra/agent-dsh 里 dsh 的入口 bin.js。
func dshBinHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".astra", "agent-dsh", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
}

// dshBinBundle 返回 bundle Resources 里 dsh 的入口 bin.js。
// 从当前可执行文件相对推导（Astra.app/Contents/MacOS → ../Resources/agent-dsh）。
// 非 bundle 环境（如 go run）推导出来的是一个不存在的路径，探针自然会跳过。
func dshBinBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), "..", "Resources", "agent-dsh",
		"node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
}

// dshStatus 检测 dsh 是否可用。返回（可用入口路径, 是否来自 bundle, 是否可用）。
//
// 顺序刻意 home 在前：DefaultAgentCommand 的解析优先级是 PATH > ~/.astra，
// 两处都在时用 home，不走 bundle 重定向，行为与运行时一致。
func dshStatus(home, bundle string) (path string, fromBundle bool, ok bool) {
	// 用户显式指定 ASTRA_AGENT 时，运行时只认它，这里不再做存在性裁决。
	if v := os.Getenv("ASTRA_AGENT"); v != "" {
		return "", false, true
	}
	// PATH 里有 dsh 直接算可用（DefaultAgentCommand 第一优先会命中它）。
	if _, err := exec.LookPath("dsh"); err == nil {
		return "", false, true
	}
	for i, p := range []string{home, bundle} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, i == 1, true
		}
	}
	return "", false, false
}

// dshAvailable 是探针的真实路径入口：本机配置为 dsh 时是否真的起得来。
func dshAvailable() bool {
	_, _, ok := dshStatus(dshBinHome(), dshBinBundle())
	return ok
}

// ── 备份 ────────────────────────────────────────────────────────────────

// backupData 把数据目录打成 zip 放到桌面。
//
// ditto 无法排除子目录，而 sandboxes/ 动辄上 GB，所以先 rsync 到临时
// 暂存区（排除 sandboxes），再由 ditto 统一打包（-k 保留 macOS 元数据）。
func backupData(src string) (string, error) {
	desktop, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(desktop, "Desktop", fmt.Sprintf("Astra-backup-%s.zip", time.Now().Format("20060102-150405")))
	if err := backupDataTo(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func backupDataTo(src, dst string) error {
	tmp, err := os.MkdirTemp("", "astra-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// 暂存到 tmp/data 再对 tmp 整体打包：ditto 打的是 src 的内容，
	// 这样 zip 解开后顶层是 data/ 而不是一堆散文件。
	stage := filepath.Join(tmp, "data")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	if b, err := exec.Command("rsync", "-a", "--exclude", "sandboxes",
		src+"/", stage+"/").CombinedOutput(); err != nil {
		return fmt.Errorf("rsync 暂存失败: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if b, err := exec.Command("ditto", "-c", "-k", "--sequesterRsrc", tmp, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("ditto 打包失败: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

func loadConfig(lg *log.Logger) desktopConfig {
	var c desktopConfig
	p := filepath.Join(appSupportDir(), "config.json")
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			lg.Printf("⚠ config.json 解析失败（%v），忽略自定义配置", err)
		}
	} else if !os.IsNotExist(err) {
		lg.Printf("⚠ 读 config.json 失败: %v", err)
	}
	// 环境变量兜底：不想把 key 写进文件的人可以 export。
	if c.LLMKey == "" {
		if k := os.Getenv("DEEPSEEK_API_KEY"); k != "" {
			c.LLMKey = k
		}
	}
	return c
}

// loadOrCreateSecret 把 JWT 密钥持久化到数据目录。
// 桌面端没有 k8s secret 机制：随机生成再存文件（0600）是标准做法，
// 否则每次重启都换密钥，所有登录态和虚拟 key 全部失效。
func loadOrCreateSecret(dataDir string, lg *log.Logger) []byte {
	p := filepath.Join(dataDir, "jwt-secret")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return b
	}
	// app.New 里的 MkdirAll 在装配时才跑，密钥写文件在它之前——自己先建。
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		lg.Printf("⚠ 无法创建数据目录（%v）：重启后所有登录态失效", err)
		return nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		fatal(lg, err)
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		lg.Printf("⚠ 无法持久化 JWT 密钥（%v）：重启后所有登录态失效", err)
	}
	return buf
}

// acquireSingleInstance 用 flock 保证只有一个桌面实例。
func acquireSingleInstance() error {
	dir := appSupportDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err := acquireFileLock(filepath.Join(dir, "desktop.lock"))
	return err
}

// ── 日志与通知 ──────────────────────────────────────────────────────────

var logFilePath string

func setupLogging() (*log.Logger, string) {
	dir := logDir()
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "astra.log")
	logFilePath = p
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	var w io.Writer = io.Discard
	if err == nil {
		w = f
	}
	return log.New(w, "astra ", log.LstdFlags|log.Lmicroseconds), p
}

func logDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "Astra")
}

// notify 发 macOS 系统通知。走 osascript 而不是 UNUserNotification：
// 不需要 entitlement 与通知权限弹窗的前置配置；失败静默——
// 通知丢了的兜底是审批卡片依然在界面里等人。
func notify(title, body string) {
	script := fmt.Sprintf(`display notification %q with title %q`, body, title)
	_ = exec.Command("osascript", "-e", script).Start()
}

func fatal(lg *log.Logger, err error) {
	lg.Printf("启动失败: %v", err)
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
