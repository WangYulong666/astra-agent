package app

// 可选能力装配：Milvus 记忆、Langfuse 观测、出网管控、调度、技能沉淀、
// 连接器、浏览器。
//
// 原则：**配了就开，起不来只记日志**。平台的核心价值是「能起沙箱跑 agent」，
// 其余是增强项——不能因为一台机器没装 Chrome 就整个起不来。
// 开关会写进 /v1/meta，前端和 CLI 据此决定是否显示入口。

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/activity"
	"github.com/WangYulong666/astra-agent/internal/api"
	"github.com/WangYulong666/astra-agent/internal/browser"
	"github.com/WangYulong666/astra-agent/internal/capability"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
	"github.com/WangYulong666/astra-agent/internal/collab"
	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/distill"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/insight"
	"github.com/WangYulong666/astra-agent/internal/memory"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/schedule"
	"github.com/WangYulong666/astra-agent/internal/skillhub"
	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/WangYulong666/astra-agent/internal/trace"
)

type capInfo struct {
	Endpoint     string
	DashboardURL string
	PublicKey    string
}

// capabilities 把七个可选能力收在一个结构里。
// 用结构体而不是七个返回值：调用点要读其中五六项，长参数列表每次加能力都要改签名。
type capabilities struct {
	Memory     memory.Store
	Trace      trace.Tracer
	TraceInfo  capInfo
	Egress     egress.Controller
	Schedules  schedule.Store
	Scheduler  schedule.Scheduler
	Distiller  distill.Distiller
	Connectors connector.Manager
	Browsers   browser.Manager
}

// runStarter 把 runner.Manager 适配成 schedule.Runner。
// 调度包不该知道 ACP / 沙箱 / 审批，它只要「给我会话和 prompt，建一个 run」。
type runStarter struct{ mgr *runner.Manager }

func (s runStarter) CreateRun(ctx context.Context, sessionID, prompt string) (string, error) {
	run, err := s.mgr.StartRun(ctx, sessionID, prompt)
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

// egCtl / connMgr 由 app.New 在 runner.New 之前打开并同时交给 runner，
// 这里只是复用同一实例——绝不能二次 Open：代理会抢同一个监听端口，
// 保管库会被两个实例各写一份，后者覆盖前者。
func openCapabilities(lg *log.Logger, st store.Store, mgr *runner.Manager, cfg Config,
	egCtl egress.Controller, connMgr connector.Manager) capabilities {
	var c capabilities
	c.Memory, c.Trace, c.TraceInfo = openObserve(lg)
	c.Egress = egCtl
	c.Schedules, c.Scheduler = openScheduler(lg, mgr, cfg)
	c.Distiller = openDistill(lg, st)
	c.Connectors = connMgr
	c.Browsers = openBrowser(lg)
	return c
}

func openObserve(lg *log.Logger) (memory.Store, trace.Tracer, capInfo) {
	var info capInfo

	memStore, err := memory.Open(memory.Config{
		Addr:         os.Getenv("MILVUS_ADDR"),
		DB:           envOr("MILVUS_DB", "default"),
		Collection:   envOr("MILVUS_COLLECTION", "astra_memory"),
		EmbedBaseURL: os.Getenv("MEMORY_EMBED_BASE_URL"),
		EmbedAPIKey:  os.Getenv("MEMORY_EMBED_API_KEY"),
		EmbedModel:   envOr("MEMORY_EMBED_MODEL", "text-embedding-3-small"),
		Dim:          envInt("MEMORY_EMBED_DIM", 0),
	})
	switch {
	case err != nil:
		lg.Printf("长期记忆未启用: %v", err)
		memStore = memory.Disabled()
	case memory.IsDisabled(memStore):
	default:
		lg.Printf("长期记忆已启用: Milvus %s (collection=%s)", os.Getenv("MILVUS_ADDR"), envOr("MILVUS_COLLECTION", "astra_memory"))
	}

	tracer, err := trace.Open(trace.Config{
		Endpoint:   os.Getenv("LANGFUSE_HOST"),
		PublicKey:  os.Getenv("LANGFUSE_PUBLIC_KEY"),
		SecretKey:  os.Getenv("LANGFUSE_SECRET_KEY"),
		SampleRate: envFloat("LANGFUSE_SAMPLE_RATE", 1),
	})
	if err != nil {
		lg.Printf("观测未启用: %v", err)
		tracer = trace.Disabled()
	} else if tracer.Enabled() {
		lg.Printf("观测已启用: Langfuse %s", os.Getenv("LANGFUSE_HOST"))
	}

	info.Endpoint = os.Getenv("LANGFUSE_HOST")
	info.PublicKey = os.Getenv("LANGFUSE_PUBLIC_KEY")
	info.DashboardURL = envOr("LANGFUSE_DASHBOARD_URL", info.Endpoint)
	return memStore, tracer, info
}

// openEgress 只有显式配了监听地址才打开——没有地址就没有代理，
// 沙箱也不会拿到 HTTP_PROXY，等于保持现状（直连）。
func openEgress(lg *log.Logger, approver egress.Approver, tokens egress.TokenProvider) egress.Controller {
	addr := os.Getenv("EGRESS_LISTEN_ADDR")
	if addr == "" {
		return nil
	}
	ctl, err := egress.Open(egress.Config{
		ListenAddr:    addr,
		DefaultPolicy: egress.DefaultPolicy(),
		Approver:      approver,
		TokenProvider: tokens,
		AskTimeout:    5 * time.Minute,
	})
	if err != nil {
		lg.Printf("出网管控未启用: %v", err)
		return nil
	}
	lg.Printf("出网管控已启用: 代理 %s（默认 deny，按租户白名单放行）", ctl.Addr())
	return ctl
}

func openScheduler(lg *log.Logger, mgr *runner.Manager, cfg Config) (schedule.Store, schedule.Scheduler) {
	if !envBool("SCHEDULER_ENABLED", true) {
		return nil, nil
	}
	dir := filepath.Join(cfg.DataDir, "schedules")
	st, sched, err := schedule.Open(schedule.Config{
		Enabled:      true,
		Tick:         30 * time.Second,
		MaxPerTenant: envInt("SCHEDULER_MAX_PER_TENANT", 50),
	}, runStarter{mgr: mgr}, dir)
	if err != nil {
		lg.Printf("定时任务未启用: %v", err)
		return nil, nil
	}
	sched.Start()
	lg.Printf("定时任务已启用（数据在 %s）", dir)
	return st, sched
}

func openDistill(lg *log.Logger, st store.Store) distill.Distiller {
	if !envBool("DISTILL_ENABLED", true) {
		return nil
	}
	dir := envOr("SKILL_DIR", filepath.Join(homeDir(), ".astra", "skills"))
	d, err := distill.Open(distill.Config{Enabled: true, SkillDir: dir, MaxSteps: 40}, st)
	if err != nil {
		lg.Printf("技能沉淀未启用: %v", err)
		return nil
	}
	lg.Printf("技能沉淀已启用（落地到 %s）", dir)
	return d
}

func openConnectors(lg *log.Logger, sidecarBase string) connector.Manager {
	key := os.Getenv("ASTRA_VAULT_KEY")
	if key == "" {
		return nil // 没有主密钥就不做：明文存 token 不如不存
	}
	m, err := connector.Open(connector.Config{
		Enabled:            true,
		VaultKey:           key,
		VaultPath:          envOr("ASTRA_VAULT_PATH", filepath.Join(homeDir(), ".astra", "vault.json")),
		BaseURL:            os.Getenv("ASTRA_BASE_URL"),
		InjectMode:         connector.InjectMode(envOr("CONNECTOR_INJECT_MODE", "proxy")),
		SidecarBase:        sidecarBase,
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		NotionClientID:     os.Getenv("NOTION_CLIENT_ID"),
		NotionClientSecret: os.Getenv("NOTION_CLIENT_SECRET"),
		LinearClientID:     os.Getenv("LINEAR_CLIENT_ID"),
		LinearClientSecret: os.Getenv("LINEAR_CLIENT_SECRET"),
	})
	if err != nil {
		lg.Printf("连接器未启用: %v", err)
		return nil
	}
	lg.Printf("连接器已启用（凭据库 %s）", os.Getenv("ASTRA_VAULT_PATH"))
	return m
}

func openBrowser(lg *log.Logger) browser.Manager {
	if !envBool("BROWSER_ENABLED", true) {
		return nil
	}
	m, err := browser.Open(browser.Config{
		Enabled:     true,
		ChromePath:  os.Getenv("CHROME_PATH"),
		MaxSessions: envInt("BROWSER_MAX_SESSIONS", 4),
		Headless:    envBool("BROWSER_HEADLESS", true),
	})
	if err != nil {
		lg.Printf("浏览器未启用: %v", err)
		return nil
	}
	lg.Printf("浏览器已启用（agent 可通过 CDP 连接）")
	return m
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func boolLabel(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// openCollab 装配多 agent 协作。桩阶段返回错误，app 层降级为 nil。
func openCollab(lg *log.Logger, mgr *runner.Manager, st store.Store, cfg Config) collab.Coordinator {
	if !envBool("COLLAB_ENABLED", true) {
		return nil
	}
	dir := filepath.Join(cfg.DataDir, "collab")
	c, err := collab.Open(collab.Config{
		Sessions: sessionsAdapter{mgr: mgr},
		DataDir:  dir,
		MaxDepth: envInt("COLLAB_MAX_DEPTH", 2),
		Timeout:  time.Duration(envInt("COLLAB_TIMEOUT_MIN", 5)) * time.Minute,
	}, st)
	if err != nil {
		lg.Printf("多 agent 协作未启用: %v", err)
		return nil
	}
	lg.Printf("多 agent 协作已启用（最大派生深度 %d，数据 %s）",
		envInt("COLLAB_MAX_DEPTH", 2), dir)
	return c
}

// openActivity 装配用户视角行动账本。
func openActivity(lg *log.Logger, st store.Store, egCtl egress.Controller,
	coord collab.Coordinator) activity.Ledger {
	var src activity.Sources
	if egCtl != nil {
		src.Egress = egressLogAdapter{ctl: egCtl}
	}
	if coord != nil {
		src.Children = childListerAdapter{coord: coord}
	}
	l, err := activity.Open(st, src)
	if err != nil {
		lg.Printf("行动账本未启用: %v", err)
		return nil
	}
	lg.Printf("行动账本已启用")
	return l
}

// ── 自闭环装配 ────────────────────────────────────────────────

// capabilitySources 把四类事实汇总成能力注册表的输入。
// 放在 app 层是为了避免 capability 反向 import api / skillhub / clikit
// （那会形成装配环），也便于单点替换来源。
type capabilitySources struct {
	skills   skillhub.Hub
	switches map[string]bool
}

func (c capabilitySources) Collect(ctx context.Context) ([]capability.Spec, error) {
	var out []capability.Spec

	routes, err := api.SpecRoutes()
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		env := requiredEnvForTag(r.Tag, r.Path)
		enabled := true
		for _, e := range env {
			if os.Getenv(e) == "" && !c.switches[envToSwitch[e]] {
				enabled = false
			}
		}
		out = append(out, capability.Spec{
			Name: r.OperationID, Kind: "api", Description: r.Summary,
			Endpoint: r.Method + " " + r.Path, Source: "openapi",
			RequiredEnv: env, Enabled: enabled,
		})
	}

	for _, cmd := range clikit.KnownCommands() {
		var env []string
		enabled := true
		for _, req := range cmd.Requires {
			env = append(env, switchToEnv[req]...)
			if c.switches[req] == false && len(switchToEnv[req]) > 0 {
				enabled = false
			}
		}
		out = append(out, capability.Spec{
			Name: cmd.CLI + " " + cmd.Name, Kind: "cli", Description: cmd.Description,
			Endpoint: cmd.Usage, Source: cmd.CLI,
			RequiredEnv: env, Requires: cmd.Requires, Enabled: enabled,
		})
	}

	if c.skills != nil {
		items, err := c.skills.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, sk := range items {
			out = append(out, capability.Spec{
				Name: sk.Name, Kind: "skill", Description: sk.Description,
				Source: sk.Source, Requires: sk.Requires,
				Enabled: sk.Status == "ready", Body: sk.Body,
			})
		}
	}
	return out, nil
}

// 能力 → 环境变量的静态映射。刻意只列「缺了就必然不可用」的必填项，
// 可选调优项（采样率、超时、上限）不进这里，否则 enabled 会一直误报 off。
var switchToEnv = map[string][]string{
	"memory":     {"MILVUS_ADDR"},
	"trace":      {"LANGFUSE_HOST", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"},
	"egress":     {"EGRESS_LISTEN_ADDR"},
	"connectors": {"ASTRA_VAULT_KEY"},
	"scheduler":  {},
	"distill":    {},
	"browser":    {},
	"collab":     {},
	"activity":   {},
}

var envToSwitch = map[string]string{
	"MILVUS_ADDR": "memory", "LANGFUSE_HOST": "trace",
	"LANGFUSE_PUBLIC_KEY": "trace", "LANGFUSE_SECRET_KEY": "trace",
	"EGRESS_LISTEN_ADDR": "egress", "ASTRA_VAULT_KEY": "connectors",
}

func requiredEnvForTag(tag, path string) []string {
	switch tag {
	case "Memory":
		return switchToEnv["memory"]
	case "Observability":
		return switchToEnv["trace"]
	case "Egress":
		return switchToEnv["egress"]
	case "Connectors":
		return switchToEnv["connectors"]
	}
	// 少数散落在别的 tag 下但依赖可选能力的端点，按路径兜一层。
	switch {
	case strings.HasPrefix(path, "/v1/admin/egress/"):
		return switchToEnv["egress"]
	case strings.HasPrefix(path, "/v1/memory/"):
		return switchToEnv["memory"]
	}
	return nil
}

func resolveCLIBin(cfg Config, lg *log.Logger) string {
	if v := os.Getenv("ASTRA_CLI_BIN"); v != "" {
		return v
	}
	cands := []string{filepath.Join(cfg.DataDir, "bin", "astra-cli")}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "astra-cli"))
	}
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	lg.Printf("⚠ 未找到 astra-cli 二进制：skill 的 requires 会标记为 unavailable")
	return ""
}

func openSkillHub(lg *log.Logger, cfg Config, hub *eventbus.Hub,
	switches map[string]bool, cliBin string) skillhub.Hub {
	dir := envOr("SKILL_DIR", filepath.Join(homeDir(), ".astra", "skills"))
	m, err := skillhub.Open(skillhub.Config{
		SkillDir: dir, CLIBin: cliBin, Switches: switches,
	}, hub)
	if err != nil {
		lg.Printf("技能注册表未启用: %v", err)
		return nil
	}
	lg.Printf("技能注册表已启用（目录 %s）", dir)
	return m
}

func openCapabilityRegistry(lg *log.Logger, skills skillhub.Hub,
	switches map[string]bool) capability.Registry {
	r, err := capability.Open(capabilitySources{skills: skills, switches: switches})
	if err != nil {
		lg.Printf("能力注册表未启用: %v", err)
		return nil
	}
	lg.Printf("能力注册表已启用")
	return r
}

type insightActivity struct{ ledger activity.Ledger }

func (a insightActivity) Summary(ctx context.Context, runID string) (insight.Summary, error) {
	if a.ledger == nil {
		return insight.Summary{}, nil
	}
	sm, err := a.ledger.Summary(ctx, runID)
	if err != nil {
		return insight.Summary{}, err
	}
	return insight.Summary{
		Done: sm.Done, Running: sm.Running, Blocked: sm.Blocked,
		Failed: sm.Failed, Current: sm.Current,
	}, nil
}

func openInsights(lg *log.Logger, st store.Store, ledger activity.Ledger,
	hub *eventbus.Hub, cfg Config) (insight.Store, func()) {
	dir := filepath.Join(cfg.DataDir, "insights")
	s, err := insight.Open(dir)
	if err != nil {
		lg.Printf("失败诊断未启用: %v", err)
		return nil, func() {}
	}
	stop := insight.Attach(hub, st, insightActivity{ledger: ledger}, dir, lg)
	lg.Printf("失败诊断已启用（数据 %s）", dir)
	return s, stop
}
