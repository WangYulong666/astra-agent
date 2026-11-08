// Package app 把「一个 astra 控制面」的完整装配收拢成可复用的库。
//
// 为什么要有这个包：cmd/astra-server 和桌面端（cmd/astra-desktop）需要
// **同一种东西**——存储 + 沙箱 + runner + 网关 + HTTP——但入口形态不同：
// 服务器由 flag 驱动、监听固定端口；桌面端由图形界面的设置驱动、监听内核
// 分配的端口。把装配逻辑留在 main 里，第二个入口只能靠复制粘贴，两边必然漂移。
//
// 职责边界：这个包不做 flag 解析、不装信号处理、不管进程退出码——
// 那是入口的事。它只负责「把配置变成一个在跑的服务」。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"

	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/api"
	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/distill"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/lifecycle"
	"github.com/WangYulong666/astra-agent/internal/llmwire"
	"github.com/WangYulong666/astra-agent/internal/memory"
	"github.com/WangYulong666/astra-agent/internal/obs"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/WangYulong666/astra-agent/internal/store/sqlstore"
	"github.com/WangYulong666/astra-agent/internal/trace"
)

// Config 是控制面的全部可调参数。零值不合法，入口必须显式给。
//
// 持续时间字段零值语义见各字段注释：Duration 类的 0 都表示「用默认」，
// 只有调用方显式传负值才关闭对应能力——跟 flag 层「传 0 关闭」不同，
// 库层面 0 更常见的含义是「没填」。这个差别写在这里，别在两个入口里猜。
type Config struct {
	// Addr 监听地址。"127.0.0.1:0" 表示让内核分配端口，Start 后用返回值。
	Addr string
	// DataDir 数据根：sqlite 库、沙箱工作区、价格表都在它下面。
	DataDir string

	// Provider 沙箱后端：local / applecontainer / cube。
	Provider string
	// Agent agent 选择：shell / dsh / 自定义命令行。
	Agent string
	// AgentEnv 传给 agent 的环境变量（凭据走这里，不进进程全局环境）。
	AgentEnv map[string]string
	// Policy 权限预设：ask / workspace-write / allow-all / deny-all。
	Policy string
	// Cwd 会话初始工作目录，空则用沙箱 workspace。
	Cwd    string
	Logger *log.Logger

	// StoreKind 存储后端：sqlite / postgres / file。
	StoreKind string
	// DSN 数据库 DSN；空则 sqlite 用 <DataDir>/astra.db。
	DSN string
	// JWTSecret 签名密钥。空则随机生成（重启后所有 token 失效）并告警。
	JWTSecret []byte
	// RequireAuth false 时不带凭据也放行（灰度期用）。
	RequireAuth bool
	// Bootstrap 无租户时自动播种默认租户与管理员（命令行模式的便利）。
	// 桌面端必须关掉：密码打在 stdout 上，.app 里的用户根本看不到——
	// 首启改走 Web 向导（bootstrap API），账号密码由用户自己定。
	Bootstrap bool

	// LLMGateway 开启 LLM 网关：真 key 只留在控制面内存。
	LLMGateway  bool
	LLMUpstream string
	LLMKey      string
	LLMProvider string
	// LLMPublicURL 网关对 **agent 视角**可达的根地址。空则不做虚拟 key 注入。
	LLMPublicURL string
	// LLMKeyTTL 虚拟 key 有效期。0 用默认 2h；负值非法。
	LLMKeyTTL time.Duration

	// Sweep / Idle / MaxAge 沙箱回收参数。0 用默认；负值关闭对应能力。
	Sweep, Idle, MaxAge time.Duration
	// Quota 每租户并发沙箱上限。0 不限。
	Quota int
}

// 默认值集中在这里，flag 层的默认值必须与它们一致——
// flag 只是把这些值先填进 Config，两处不一致时以这里为准并修 flag。
const (
	DefaultSweep  = 5 * time.Minute
	DefaultIdle   = 30 * time.Minute
	DefaultMaxAge = 6 * time.Hour
	DefaultKeyTTL = 2 * time.Hour
)

// Server 是一个装配完成、可以 Start 的控制面。
type Server struct {
	cfg    Config
	lg     *log.Logger
	st     store.Store
	admin  store.AdminStore
	mgr    *runner.Manager
	prov   sandbox.Provider
	hub    *eventbus.Hub
	reaper *lifecycle.Reaper
	auth   *auth.Service
	// 全局事件旁路的停止函数（trace 上报、记忆入库）。
	// 必须在关存储之前调用：它们可能还在写，先停再关。
	stopTrace   func()
	stopMem     func()
	stopInsight func()
	// 自动沉淀订阅（run 终态 → skill 草稿），关停时要等在飞任务退出。
	stopAutoDistill func()
	// 装配后期才确定的运行时信息，runner 持同一指针。
	hints *runner.RuntimeHints
	// tracer 是观测出口本体；stopTrace 只停事件桥，不关 worker，
	// 退出前还要单独调 tracer.Close()（顺序见 Close）。可能是 Disabled 的空实现。
	tracer trace.Tracer

	meta map[string]string

	// LLM 网关运行态。llmSet 非 nil 表示设置接口可用。
	llmKey      string
	llmBase     string
	llmProvider string
	llmOn       bool
	llmSet      *llmSettings

	ln     net.Listener
	srv    *http.Server
	errCh  chan error
	closed bool
}

// New 装配一切但不监听。返回前已完成数据库迁移与中断任务归档，
// 所以桌面端可以在 New 成功后再决定窗口里展示什么。
func New(cfg Config) (*Server, error) {
	lg := cfg.Logger
	if lg == nil {
		lg = log.New(io.Discard, "astra ", log.LstdFlags|log.Lmicroseconds)
	}
	if cfg.DataDir == "" {
		return nil, errors.New("app: DataDir 不能为空")
	}
	// 数据目录可能还不存在（首启、新机器）。sqlite 打不开不存在的目录，
	// 报错信息（unable to open database file）又完全看不出是目录的事，先建。
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", cfg.DataDir, err)
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	s := &Server{cfg: cfg, lg: lg, errCh: make(chan error, 1), llmProvider: cfg.LLMProvider}

	st, adminStore, err := openStore(cfg.StoreKind, cfg.DSN, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s.st, s.admin = st, adminStore
	s.hub = eventbus.New()

	prov, err := sandbox.New(cfg.Provider, sandbox.Options{
		Root:     filepath.Join(cfg.DataDir, "sandboxes"),
		ProxyBin: sandbox.ResolveProxyBin(""),
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	s.prov = prov

	spec, err := resolveAgent(cfg.Agent, cfg.AgentEnv)
	if err != nil {
		st.Close()
		return nil, err
	}

	// ── 认证 ──
	var authenticator auth.Authenticator
	var auditor api.Auditor
	if adminStore != nil {
		secret := cfg.JWTSecret
		if len(secret) == 0 {
			// 未显式指定时从数据目录加载；没有就生成一次并落盘（0600）。
			// 早先只随机生成不落盘，导致命令行模式每次重启都把所有人踢下线——
			// 桌面端自己落盘过，这里下沉成统一行为，两个入口才真正一致。
			var err error
			secret, err = loadOrCreateSecret(cfg.DataDir, lg)
			if err != nil {
				st.Close()
				return nil, fmt.Errorf("准备 JWT 密钥: %w", err)
			}
		}
		svc, err := auth.New(auth.Config{Store: adminStore, Secret: secret})
		if err != nil {
			st.Close()
			return nil, err
		}
		s.auth = svc
		authenticator = svc
		aud := obs.NewAuditor(adminStore, obs.New("info", os.Stderr))
		auditor = aud
		// 首次启动自动播种：否则登录页永远进不去（没有账号可登录）。
		// 只在 Bootstrap 开着时做；桌面端走 Web 向导。
		if cfg.Bootstrap {
			if err := bootstrapIfEmpty(lg, adminStore); err != nil {
				lg.Printf("初始化租户失败: %v", err)
			}
		}
	}

	// ── 会话级虚拟 key：真 key 不进沙箱 ──
	var creds runner.CredentialIssuer
	ttl := cfg.LLMKeyTTL
	if ttl == 0 {
		ttl = DefaultKeyTTL
	}
	if (cfg.LLMGateway || cfg.LLMPublicURL != "") && s.auth != nil && cfg.LLMPublicURL != "" {
		iss, err := llmwire.New(llmwire.Config{
			Auth: s.auth, PublicURL: cfg.LLMPublicURL, TTL: ttl, Logger: lg,
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		creds = iss
		// 结构性保证：既然由 Creds 注入虚拟 key，就别再把真 key 留在 spec.Env 里。
		delete(spec.Env, llmwire.EnvAPIKey)
		lg.Printf("沙箱凭据已切换为虚拟 key：agent 将直连 %s/v1/llm", cfg.LLMPublicURL)
	}
	if creds == nil && wantsDsh(cfg.Agent) && spec.Env["DEEPSEEK_API_KEY"] == "" {
		lg.Printf("⚠ 未提供 DEEPSEEK_API_KEY，dsh 会在 prompt 时返回 -32603。" +
			"在配置里填 key，或开 LLMGateway 让沙箱只拿虚拟 key")
	}

	// 出网代理与连接器必须先于 runner.New 打开：runner 在 CreateSession
	// 里按会话注入 HTTP_PROXY 并登记流量归属，装配晚了就注入不进去。
	egApprover := &lazyApprover{}
	egTokens := &lazyTokenProvider{}
	egCtl := openEgress(lg, egApprover, egTokens)
	// connector 的 proxy 模式要把 sidecar 地址给沙箱；代理没启用时传空串，
	// connector 会明确报错而不是给一个连不上的假地址。
	sidecar := ""
	if egCtl != nil {
		sidecar = egCtl.Addr()
	}
	connMgr := openConnectors(lg, sidecar)
	egBypass := egressBypassFor(cfg, lg)
	if egCtl != nil && egBypass == "" {
		lg.Printf("⚠ 出网管控已启用且 NO_PROXY 为空：沙箱连本机 LLM 网关会被私网规则拦截。" +
			"若 agent 通过本地网关取模型，请设 EGRESS_NO_PROXY=127.0.0.1:<port>")
	}

	// 沙箱凭据签发器用接口断言：*auth.Service 由 T4 加上 IssueSandboxKey，
	// 没加之前自动降级为 nil（兼容模式），骨架也能编译。
	var sandboxIssuer runner.SandboxKeyIssuer
	if s.auth != nil {
		if v, ok := any(s.auth).(runner.SandboxKeyIssuer); ok {
			sandboxIssuer = v
		}
	}
	hints := runner.NewRuntimeHints()
	s.hints = hints

	mgr, err := runner.New(runner.Config{
		Store:            st,
		Provider:         prov,
		Hub:              s.hub,
		Agent:            spec,
		Policy:           runner.PolicyByName(cfg.Policy),
		Logger:           lg,
		Creds:            creds,
		Egress:           egCtl,
		Connectors:       connMgr,
		EgressBypass:     egBypass,
		NetworkLock:      networkLockEnabled(lg, egCtl),
		Hints:            hints,
		SandboxKeyIssuer: sandboxIssuer,
		InsightReplay:    envBool("INSIGHT_REPLAY", true),
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	s.mgr = mgr

	// 到这里 runner / connector 都活了，把延迟适配器填实。
	// 之前它们是空壳，行为保守（审批 deny、token 未授权），不会误放行。
	egApprover.Set(approverAdapter{mgr: mgr})
	if connMgr != nil {
		egTokens.Set(tokenAdapter{mgr: connMgr})
	}

	// 上一个进程可能被直接杀掉，留下停在 running 的 run。先归档再接客。
	if n, err := mgr.RecoverInterrupted(context.Background()); err != nil {
		lg.Printf("归档中断任务失败: %v", err)
	} else if n > 0 {
		lg.Printf("已归档 %d 个上次未结束的 run", n)
	}

	// ── LLM 网关 ──
	// key 的解析顺序：显式配置 > 环境变量 > 向导落盘的 dataDir/llm-key.json。
	// 最后一档是给桌面端首启向导用的：用户在 Web 界面里填的 key 必须活过重启。
	key := cfg.LLMKey
	if key == "" {
		key = os.Getenv("DEEPSEEK_API_KEY")
	}
	base := cfg.LLMUpstream
	if saved, err := loadLLMKeyFile(cfg.DataDir); err == nil && saved.APIKey != "" {
		if key == "" {
			key = saved.APIKey
		}
		if saved.BaseURL != "" && cfg.LLMUpstream == "" {
			base = saved.BaseURL
		}
	}
	// 网关始终装配（有治理存储的前提下）：key 可能在启动时还不存在——
	// 桌面端首启向导是「先起服务、再在界面里填 key」。若只在启动时有 key
	// 才开网关，向导保存的 key 就没有任何地方消费，「填了就能用」不成立。
	// 无 key 时网关的行为是明确报错，不是静默泄漏。
	gwOn := adminStore != nil
	if gwOn {
		if adminStore == nil {
			lg.Printf("⚠ LLMGateway 需要治理层存储才能记账，已忽略")
		} else {
			s.applyGateway(key, base)
			s.llmSet = &llmSettings{srv: s, dataDir: cfg.DataDir, lg: lg}
			if key == "" {
				lg.Printf("⚠ LLMGateway 已开启但没有上游 key，网关会回 502；可在设置里填")
			}
		}
	}

	// ── 沙箱生命周期治理 ──
	sweep, idle, maxAge := cfg.Sweep, cfg.Idle, cfg.MaxAge
	if sweep == 0 {
		sweep = DefaultSweep
	}
	if idle == 0 {
		idle = DefaultIdle
	}
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}
	lcPolicy := lifecycle.Policy{
		SweepInterval: sweep, IdleTimeout: idle, MaxAge: maxAge, MaxPerTenant: cfg.Quota,
	}
	s.reaper = lifecycle.New(lcPolicy, lifecycle.Deps{Provider: prov, Store: st, Logger: lg})
	go s.reaper.Run(context.Background())

	var quota *lifecycle.Quota
	if cfg.Quota > 0 {
		quota = &lifecycle.Quota{Provider: prov, Store: st, MaxPerTenant: cfg.Quota}
	}

	// 可选能力：配了就开、没配就关。任何一项起不来都只记日志，
	// 不能让「没装 Milvus」变成「平台起不来」。
	capa := openCapabilities(lg, st, mgr, cfg, egCtl, connMgr)
	s.tracer = capa.Trace
	// 自动沉淀：run 终态且成功工具调用 ≥2 时自动生成草稿，人仍保留 accept 权。
	// 必须在 Hub 建好、Distiller 装好之后挂；distill 未启用时 Attach 自身 noop。
	s.stopAutoDistill = distill.Attach(s.hub, capa.Distiller, st, lg)
	coord := openCollab(lg, mgr, st, cfg)
	ledger := openActivity(lg, st, egCtl, coord)
	// 两个旁路都订阅 hub 的全局事件。Hub 已支持多消费者（追加语义），
	// 谁先装配都不会把对方顶掉。
	s.stopTrace = trace.Attach(s.hub, capa.Trace, st)
	s.stopMem = memory.AttachWith(capa.Memory, s.hub, st, lg)

	info := map[string]string{
		"provider":  prov.Name(),
		"agent":     strings.Join(spec.Command, " "),
		"agentName": agentLabel(cfg.Agent, spec),
		"policy":    cfg.Policy,
		"approval":  approvalGranularity(cfg.Agent, cfg.Policy),
		// 让前端/CLI 一眼看出哪些能力关着，而不是点了报 503 才发现
		"memory":     boolLabel(!memory.IsDisabled(capa.Memory)),
		"trace":      boolLabel(capa.Trace.Enabled()),
		"egress":     boolLabel(capa.Egress != nil),
		"scheduler":  boolLabel(capa.Scheduler != nil),
		"distill":    boolLabel(capa.Distiller != nil),
		"connectors": boolLabel(capa.Connectors != nil),
		"browser":    boolLabel(capa.Browsers != nil),
		"collab":     boolLabel(coord != nil),
		"activity":   boolLabel(ledger != nil),
	}
	// ── 自闭环装配：能力注册表 / 技能注册表 / 失败诊断 ──
	// 放在 info 之后，因为 switches 直接复用同一份能力开关，避免两处漂移。
	switches := make(map[string]bool, len(info))
	for k, v := range info {
		switches[k] = v == "on"
	}
	cliBin := resolveCLIBin(cfg, lg)
	skillHub := openSkillHub(lg, cfg, s.hub, switches, cliBin)
	caps := openCapabilityRegistry(lg, skillHub, switches)
	insights, stopInsight := openInsights(lg, st, ledger, s.hub, cfg)
	s.stopInsight = stopInsight
	hints.Set(insights, "http://"+cfg.Addr, cliBin)
	info["capabilities"] = boolLabel(caps != nil)
	info["skills"] = boolLabel(skillHub != nil)
	info["insights"] = boolLabel(insights != nil)

	h, err := api.Handler(api.Config{
		Manager: mgr, Store: st, Hub: s.hub, Provider: prov, Logger: lg,
		Auth: authenticator, Admin: adminStore, Auditor: auditor,
		RequireAuth: cfg.RequireAuth,
		DefaultCwd:  cfg.Cwd,
		Reaper:      s.reaper,
		Quota:       quota,
		Info:        info,
		LLMSettings: s.LLMSettings(),
		Memory:      capa.Memory, Trace: capa.Trace,
		TraceEndpoint: capa.TraceInfo.Endpoint, TraceDashboardURL: capa.TraceInfo.DashboardURL,
		TracePublicKey: capa.TraceInfo.PublicKey,
		Egress:         capa.Egress, Schedules: capa.Schedules, Scheduler: capa.Scheduler,
		Distiller: capa.Distiller, Connectors: capa.Connectors, Browsers: capa.Browsers,
		Collab: coord, Ledger: ledger,
		Capabilities: caps, SkillHub: skillHub, Insights: insights,
	})
	if err != nil {
		st.Close()
		return nil, err
	}
	s.meta = info

	s.srv = &http.Server{
		Addr: cfg.Addr,
		// 指标包在最外层：CORS 预检、panic 兜底的 5xx 也计入。
		Handler:           api.WrapMetrics(h),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：SSE 是长连接，靠 ResponseController 逐条控制。
	}
	return s, nil
}

// Start 监听并后台 serve。返回实际监听地址——Config.Addr 端口为 0 时由内核分配。
// serve 期的错误（进程级崩溃）从 Err() 读；HTTP 层的业务错误不在这条通道里。
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return "", err
	}
	s.ln = ln
	// cfg.Addr 可能是 :0（内核分配端口），到 Start 才知道真实地址。
	// 沙箱里的 ASTRA_SERVER 必须用这个，否则 astra-cli 连不上。
	if s.hints != nil {
		s.hints.SetURL("http://" + ln.Addr().String())
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errCh <- err
		}
	}()
	s.lg.Printf("控制面就绪 http://%s  provider=%s data=%s", ln.Addr(), s.prov.Name(), s.cfg.DataDir)
	return ln.Addr().String(), nil
}

// Err 返回 serve 期错误通道。Start 之后读；收到非 nil 错误说明服务已停。
func (s *Server) Err() <-chan error { return s.errCh }

// Meta 服务自述（provider / agent / policy / approval 粒度）。
func (s *Server) Meta() map[string]string { return s.meta }

// Hub 事件总线，供进程内消费者挂全局回调（如桌面通知）。
func (s *Server) Hub() *eventbus.Hub { return s.hub }

// LLMSettings 上游凭据的热更接口，传给 HTTP 层。未启用时返回 nil。
func (s *Server) LLMSettings() api.LLMSettingsStore {
	if s.llmSet == nil {
		return nil
	}
	return s.llmSet
}

// Shutdown 优雅停机：先停 HTTP，再关 runner（不销毁沙箱，重启后可接管）。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	return s.mgr.Close()
}

// Close 释放底层存储。Shutdown 之后调用。
func (s *Server) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// 先停旁路：trace 要把缓冲刷出去、记忆要等在飞的写入落库，
	// 都依赖存储还活着，所以顺序不能反。
	if s.stopTrace != nil {
		s.stopTrace() // 停事件桥并 Flush 已入队事件
	}
	// tracer.Close 不能省：stopTrace 只停事件桥，不关 Langfuse 的 worker；
	// 少了这一步 worker goroutine 会泄漏，队列/事件桥的丢弃计数也永远没人读。
	// 必须在 stopTrace 之后（桥停了不会再往队列写）才能 drain 完最后一批，
	// 又要在关存储之前，顺序不能反。Disabled().Close() 是无害的 no-op。
	if s.tracer != nil {
		if err := s.tracer.Close(); err != nil {
			s.lg.Printf("关闭观测上报失败: %v", err)
		}
	}
	if s.stopMem != nil {
		s.stopMem()
	}
	// 诊断订阅要在关存储前停：它可能还在写 insights.json。
	if s.stopInsight != nil {
		s.stopInsight()
	}
	if s.stopAutoDistill != nil {
		s.stopAutoDistill()
	}
	return s.st.Close()
}

// ── 以下是从 cmd/astra-server/main.go 搬进来的装配细节 ──────────────────

func openStore(kind, dsn, dir string) (store.Store, store.AdminStore, error) {
	switch kind {
	case "file":
		fs, err := store.OpenFile(filepath.Join(dir, "store"), store.Options{Sync: true})
		if err != nil {
			return nil, nil, err
		}
		return fs, nil, nil
	case "sqlite":
		if dsn == "" {
			dsn = "sqlite://" + filepath.Join(dir, "astra.db")
		}
		ss, err := sqlstore.Open(dsn)
		if err != nil {
			return nil, nil, err
		}
		return ss, ss, nil
	case "postgres":
		if dsn == "" {
			return nil, nil, errors.New("store postgres 需要 DSN（如 postgres://user:pass@host:5432/astra）")
		}
		ss, err := sqlstore.Open(dsn)
		if err != nil {
			return nil, nil, err
		}
		return ss, ss, nil
	default:
		return nil, nil, fmt.Errorf("未知存储后端 %q（可选 sqlite / postgres / file）", kind)
	}
}

func wantsDsh(v string) bool {
	v = strings.TrimSpace(v)
	return v == "dsh" || strings.Contains(v, "--profile acp")
}

// resolveAgent 把 agent 选择翻译成具体命令。shell 是开发默认：指向与
// 服务二进制同目录的 acp-shell-agent——「同目录」是可移植性，不依赖 PATH。
func resolveAgent(v string, env map[string]string) (sandbox.AgentSpec, error) {
	v = strings.TrimSpace(v)
	forward := map[string]string{}
	for k, val := range env {
		if k != "" && val != "" {
			forward[k] = val
		}
	}
	switch v {
	case "", "shell":
		if p, ok := sandbox.SiblingBinary("acp-shell-agent"); ok {
			return sandbox.AgentSpec{Command: []string{p}, Env: forward}, nil
		}
		return sandbox.AgentSpec{Command: []string{"acp-shell-agent"}, Env: forward}, nil
	case "dsh":
		cmd := sandbox.DefaultAgentCommand()
		// 裁剪层必须带上：缺依赖的插件会把 import 失败警告写进 stdout，
		// 而 ACP 的 NDJSON 就在 stdout 上，一行日志就能让整条协议流解析失败。
		if p, ok := sandbox.DSHTrimPath(); ok {
			cmd = append(cmd, "--patch", p)
		}
		if _, ok := forward["DEEPSEEK_API_KEY"]; !ok {
			if k, found := os.LookupEnv("DEEPSEEK_API_KEY"); found && k != "" {
				forward["DEEPSEEK_API_KEY"] = k
			}
		}
		// dsh 自带文件级沙箱（Seatbelt / Landlock / bwrap），平台不再外面套一层。
		return sandbox.AgentSpec{Command: cmd, Env: forward, SelfSandbox: true}, nil
	default:
		return sandbox.AgentSpec{Command: strings.Fields(v), Env: forward}, nil
	}
}

// approvalGranularity 说明审批真正拦在什么粒度上。界面上写「逐条审批」
// 会让人以为每条命令都要点——dsh 只在模型申请扩权时才问人。
func approvalGranularity(agentCmd, policy string) string {
	if policy != "ask" {
		return "none"
	}
	if wantsDsh(agentCmd) {
		return "escalation"
	}
	return "per-tool"
}

// agentLabel 给界面一个短标签，不显示临时路径。
func agentLabel(input string, spec sandbox.AgentSpec) string {
	switch {
	case wantsDsh(input):
		return "dsh"
	case strings.TrimSpace(input) == "", strings.TrimSpace(input) == "shell":
		return "shell"
	}
	if len(spec.Command) == 0 {
		return "agent"
	}
	first := filepath.Base(spec.Command[0])
	return strings.TrimSuffix(first, filepath.Ext(first))
}

// bootstrapIfEmpty 在系统里一个租户都没有时播种默认租户与管理员。
// 密码随机生成并只打印一次，与 API Key 一致：明文不落库。
func bootstrapIfEmpty(lg *log.Logger, st store.AdminStore) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tenants, err := st.ListTenants(ctx)
	if err != nil {
		return err
	}
	if len(tenants) > 0 {
		return nil
	}

	tid, err := auth.RandomID()
	if err != nil {
		return err
	}
	uid, err := auth.RandomID()
	if err != nil {
		return err
	}
	pw, err := auth.GeneratePassword()
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	tenant := store.Tenant{
		ID: tid, Name: "默认租户", Slug: "default",
		Status: store.StatusActive, CreatedAt: now,
	}
	if err := st.CreateTenant(ctx, tenant); err != nil {
		return fmt.Errorf("建租户: %w", err)
	}
	owner := store.User{
		ID: uid, TenantID: tid, Email: "admin@astra.local", DisplayName: "管理员",
		PasswordHash: hash, Role: store.RoleOwner, Status: store.StatusActive, CreatedAt: now,
	}
	if err := st.CreateUser(ctx, owner); err != nil {
		return fmt.Errorf("建管理员: %w", err)
	}

	fmt.Fprintf(os.Stderr, `
┌──────────────────────────────────────────────┐
│  已初始化默认租户（只在首次启动时出现一次）   │
└──────────────────────────────────────────────┘
   租户 slug : default
   管理员    : admin@astra.local
   密码      : %s

   密码只显示这一次，请立即保存。
`, pw)
	lg.Printf("已播种默认租户 default 与管理员 admin@astra.local")
	return nil
}

// egressBypassFor 决定 NO_PROXY：显式环境变量优先，否则在 LLM 网关指向本机时
// 自动放行那一个 host:port。
//
// 为什么需要：出网代理默认 deny + 拦私网，而本地开发的 agent 常通过
// 127.0.0.1:<port>/v1/llm 取模型。全量注入 HTTP_PROXY 会把网关一起挡掉，
// 症状是「开了出网管控之后每个 run 都报模型连不上」，而且很难联想到代理。
// 只放行精确的本机网关地址，不放行整个 localhost 网段。
func egressBypassFor(cfg Config, lg *log.Logger) string {
	if v := os.Getenv("EGRESS_NO_PROXY"); v != "" {
		return v
	}
	if cfg.LLMPublicURL == "" {
		return ""
	}
	u, err := url.Parse(cfg.LLMPublicURL)
	if err != nil || u.Host == "" {
		lg.Printf("解析 LLMPublicURL %q 失败，出网例外未自动推导", cfg.LLMPublicURL)
		return ""
	}
	h := u.Hostname()
	if h == "127.0.0.1" || h == "localhost" || h == "::1" {
		return u.Host
	}
	return ""
}

// networkLockEnabled 决定要不要给沙箱套 Seatbelt 网络锁。
//
// 默认关。开启条件很硬：必须显式设 ASTRA_NETWORK_LOCK=1 且出网代理真的起来了。
// 少一个都不开——规则写错或代理没起会让所有 agent 连不上模型，报错还只是
// "Operation not permitted"，排查成本极高，不能让它成为默认行为。
func networkLockEnabled(lg *log.Logger, egCtl egress.Controller) bool {
	if !envBool("ASTRA_NETWORK_LOCK", false) {
		return false
	}
	if egCtl == nil || !egCtl.Enabled() {
		lg.Printf("⚠ 已忽略 ASTRA_NETWORK_LOCK：出网代理未启用，开了会全断网")
		return false
	}
	lg.Printf("⚠ 出网强制边界已开启：沙箱只能连 %s（DNS 与本机回环例外）", egCtl.Addr())
	return true
}
