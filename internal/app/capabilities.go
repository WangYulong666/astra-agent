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
	"time"

	"github.com/WangYulong666/astra-agent/internal/browser"
	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/distill"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/memory"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/schedule"
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

func openCapabilities(lg *log.Logger, st store.Store, mgr *runner.Manager, cfg Config) capabilities {
	var c capabilities
	c.Memory, c.Trace, c.TraceInfo = openObserve(lg)
	c.Egress = openEgress(lg)
	c.Schedules, c.Scheduler = openScheduler(lg, mgr, cfg)
	c.Distiller = openDistill(lg, st)
	c.Connectors = openConnectors(lg)
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
func openEgress(lg *log.Logger) egress.Controller {
	addr := os.Getenv("EGRESS_LISTEN_ADDR")
	if addr == "" {
		return nil
	}
	ctl, err := egress.Open(egress.Config{
		ListenAddr:    addr,
		DefaultPolicy: egress.DefaultPolicy(),
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

func openConnectors(lg *log.Logger) connector.Manager {
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
