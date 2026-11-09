// Package api 是控制面的 HTTP 入口。
//
// 只有两件事：把请求转给 runner，把事件流从 store + hub 推给客户端。
// 不做会话状态缓存——runner 与 store 才是事实来源，这里多存一份必然漂移。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/activity"
	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/browser"
	"github.com/WangYulong666/astra-agent/internal/capability"
	"github.com/WangYulong666/astra-agent/internal/collab"
	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/distill"
	"github.com/WangYulong666/astra-agent/internal/doclib"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/insight"
	"github.com/WangYulong666/astra-agent/internal/lifecycle"
	"github.com/WangYulong666/astra-agent/internal/memory"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/schedule"
	"github.com/WangYulong666/astra-agent/internal/skillhub"
	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/WangYulong666/astra-agent/internal/trace"
)

// Auditor 接收审计记录。
//
// 接口定义在这里而不是 obs 包，是为了让依赖方向保持 obs → api。
// 反过来的话，未启用可观测性时 api 包就得为了一个空接口去 import obs，
// 那个包又会 import prometheus，最后单测被迫拖一堆依赖。
type Auditor interface {
	Audit(ctx context.Context, a store.AuditLog)
}

// Config 装配 HTTP 层。
type Config struct {
	Manager  *runner.Manager
	Store    store.Store
	Hub      *eventbus.Hub
	Provider sandbox.Provider
	Logger   *log.Logger

	// Auth 认证器。nil 表示兼容模式：不校验身份、全放行，启动时打告警。
	// 保留 nil 路径是为了「clone 下来 make serve 就能用」，不被登录页挡在门外。
	Auth auth.Authenticator
	// Admin 治理层存储。nil 表示未启用租户/用户/用量/审计。
	Admin store.AdminStore
	// Auditor 审计。nil 时静默丢弃。
	Auditor Auditor
	// Memory 长期记忆（Milvus）。nil 表示未启用，API 返回 503 而不是崩。
	Memory memory.Store
	// Trace 观测（Langfuse）。nil 表示未启用。
	Trace trace.Tracer
	// TraceEndpoint / TraceDashboardURL / TracePublicKey 只用于运营页展示；
	// secret key 留在服务端，不进这个结构。
	// Egress 出网管控。nil 表示未启用（沙箱直连）。
	Egress egress.Controller
	// Schedules / Scheduler 定时与 webhook 触发。
	Schedules schedule.Store
	Scheduler schedule.Scheduler
	// Distiller 从 run 轨迹沉淀技能草稿。
	Distiller distill.Distiller
	// Connectors 第三方授权与凭据托管。
	Connectors           connector.Manager
	ConnectorRedirectURL string
	// Browsers 给 agent 用的 CDP 浏览器实例。
	Browsers browser.Manager
	// Collab 多 agent 协作（父 run 派生子 agent）；Ledger 用户视角行动账本。
	Collab collab.Coordinator
	Ledger activity.Ledger
	// 自闭环：能力注册表 / 技能注册表 / 失败诊断。
	// 三者都允许 nil（能力没配好时降级，平台照常启动）。
	Capabilities capability.Registry
	SkillHub     skillhub.Hub
	Insights     insight.Store
	// Docs 是内置文档库（含运行时生成的功能总览）。nil 时回 503。
	Docs doclib.Library

	TraceEndpoint     string
	TraceDashboardURL string
	TracePublicKey    string
	// 是否要求认证。由启动参数决定；只有 Auth 非空且 RequireAuth 为真才强制登录。
	RequireAuth bool

	// DefaultCwd 建会话未指定 cwd 时使用的目录；空则用各沙箱自己的 workspace。
	DefaultCwd string
	// Info 原样回给前端的运行信息（provider / agent / policy）。
	// 这些值只用于展示，刻意不让前端去猜——猜错比不显示更糟。
	Info map[string]string

	// LLMSettings 上游凭据的查看/热更。nil 表示网关未启用，
	// 向导里的 LLM 步骤会隐藏、设置接口回 412。
	LLMSettings LLMSettingsStore

	// Heartbeat SSE 心跳间隔。0 用 15s。
	// 必须发心跳：中间任何一层代理在 60s 静默后都会掐连接，
	// 而一次长命令可能几分钟没有输出。
	Heartbeat time.Duration

	// Reaper 沙箱回收器。nil 表示生命周期治理未启用，管理端点回 501。
	Reaper *lifecycle.Reaper
	// Quota 建会话前的准入控制。nil 表示不限并发沙箱数。
	// 与 Reaper 分开：配额是同步拒绝（创建前），回收是异步兜底（后面），
	// 只做回收不做准入会让租户在超限时先成功创建、几秒后被销毁，体验更差。
	Quota *lifecycle.Quota
}

// Handler 返回带路由的 http.Handler。
func Handler(cfg Config) (http.Handler, error) {
	if cfg.Manager == nil || cfg.Store == nil || cfg.Hub == nil {
		return nil, errors.New("api: Manager/Store/Hub 都必需")
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	lg := cfg.Logger
	if lg == nil {
		// 必须是 io.Discard 而不是 nil：log.New(nil,...) 的每次输出都会空指针 panic，
		// 而这条路径只在 500 时触发——等于「一出错就把控制面打挂」。
		lg = log.New(io.Discard, "", 0)
	}
	h := &handler{cfg: cfg, log: lg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/meta", h.authed(func(w http.ResponseWriter, r *http.Request) {
		// 必须复制一份再改：h.cfg.Info 是进程级共享 map，而每次请求都会往里面写
		// requireAuth/email。直接写共享 map 会和 setupState 这类「读同一个 map」
		// 的并发请求撞成 fatal error: concurrent map read and map write——
		// 实测首屏同时打 /v1/meta 和 /v1/setup/state 就会把控制面打挂。
		info := make(map[string]string, len(h.cfg.Info)+4)
		for k, v := range h.cfg.Info {
			info[k] = v
		}
		// 前端靠这几个字段决定「要不要显示登录页」「顶栏写谁」。
		p := h.principal(r)
		info["requireAuth"] = boolStr(h.cfg.RequireAuth && h.cfg.Auth != nil)
		if p.TenantID != "" {
			info["tenantId"] = p.TenantID
			info["email"] = p.Email
			info["role"] = p.Role
		}
		writeJSON(w, http.StatusOK, info)
	}, ""))

	mux.HandleFunc("POST /v1/sessions", h.authed(h.createSession, auth.ActionSessionCreate))
	mux.HandleFunc("GET /v1/sessions", h.authed(h.listSessions, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/sessions/{id}", h.authed(h.getSession, auth.ActionSessionRead))
	mux.HandleFunc("PATCH /v1/sessions/{id}", h.authed(h.updateSession, auth.ActionSessionRead))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.authed(h.deleteSession, auth.ActionSessionDelete))
	mux.HandleFunc("GET /v1/sessions/{id}/files", h.authed(h.listFiles, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/sessions/{id}/file", h.authed(h.readFile, auth.ActionSessionRead))
	// 下载走另一条端点：/file 是 JSON 预览（二进制不返内容、512KB 截断），
	// 拿它落盘会得到一个坏文件。见 files.go 的 downloadFile。
	mux.HandleFunc("GET /v1/sessions/{id}/download", h.authed(h.downloadFile, auth.ActionSessionRead))

	mux.HandleFunc("POST /v1/sessions/{id}/runs", h.authed(h.createRun, auth.ActionRunCreate))
	mux.HandleFunc("GET /v1/sessions/{id}/runs", h.authed(h.listRuns, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/runs/{id}", h.authed(h.getRun, auth.ActionSessionRead))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", h.authed(h.cancelRun, auth.ActionRunCancel))
	// events 不走 authed：EventSource 无法带自定义 header，
	// 需要额外接受 ?ticket= 一次性票据，见 sse_ticket.go 的 authedEvents。
	mux.HandleFunc("GET /v1/runs/{id}/events", h.authedEvents(h.events, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/runs/{id}/history", h.authed(h.history, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/runs/{id}/approvals", h.authed(h.listApprovals, auth.ActionSessionRead))
	mux.HandleFunc("POST /v1/runs/{id}/approvals/{toolCallId}", h.authed(h.resolveApproval, auth.ActionApproval))

	// ── 长期记忆（Milvus）────────────────────────────────────────
	// 全部按「自己的 tenant+user」过滤，权限只表达「能不能用这个能力」，
	// 不表达「能不能读别人」——后者在设计上就不存在。
	mux.HandleFunc("POST /v1/memory/ingest", h.authed(h.memoryIngest, auth.ActionMemoryWrite))
	mux.HandleFunc("GET /v1/memory/search", h.authed(h.memorySearch, auth.ActionMemoryRead))
	mux.HandleFunc("GET /v1/memory/stats", h.authed(h.memoryStats, auth.ActionMemoryRead))
	mux.HandleFunc("DELETE /v1/memory/{id}", h.authed(h.memoryDelete, auth.ActionMemoryWrite))

	// ── 观测（Langfuse）──────────────────────────────────────────
	mux.HandleFunc("GET /v1/admin/obs/config", h.authed(h.obsConfig, auth.ActionObsRead))
	mux.HandleFunc("GET /v1/admin/obs/traces", h.authed(h.obsTraces, auth.ActionObsRead))

	// ── 出网管控 ────────────────────────────────────────────────
	// 读策略给所有登录用户（他要知道自己能不能装包），改策略只给管租户的人。
	mux.HandleFunc("GET /v1/admin/egress/policy", h.authed(h.egressPolicy, auth.ActionSessionRead))
	mux.HandleFunc("PUT /v1/admin/egress/policy", h.authed(h.putEgressPolicy, auth.ActionTenantManage))
	mux.HandleFunc("GET /v1/admin/egress/log", h.authed(h.egressLog, auth.ActionAuditRead))

	// ── 定时任务 / webhook 触发 ─────────────────────────────────
	mux.HandleFunc("GET /v1/schedules", h.authed(h.listSchedules, auth.ActionSessionRead))
	mux.HandleFunc("POST /v1/schedules", h.authed(h.createSchedule, auth.ActionRunCreate))
	mux.HandleFunc("GET /v1/schedules/{id}", h.authed(h.getSchedule, auth.ActionSessionRead))
	mux.HandleFunc("PATCH /v1/schedules/{id}", h.authed(h.patchSchedule, auth.ActionRunCreate))
	mux.HandleFunc("DELETE /v1/schedules/{id}", h.authed(h.deleteSchedule, auth.ActionRunCreate))
	mux.HandleFunc("POST /v1/schedules/{id}/run", h.authed(h.runScheduleNow, auth.ActionRunCreate))
	// webhook 入口不经过 authed：调用方是外部系统，靠 URL 里的 token 鉴权
	mux.HandleFunc("POST /v1/hooks/{token}", h.hookTrigger)

	// ── 技能沉淀 ────────────────────────────────────────────────
	mux.HandleFunc("POST /v1/runs/{id}/distill", h.authed(h.distillRun, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/skills/drafts", h.authed(h.listSkillDrafts, auth.ActionSessionRead))
	mux.HandleFunc("POST /v1/skills/drafts/{id}/accept", h.authed(h.acceptSkillDraft, auth.ActionSessionCreate))
	mux.HandleFunc("POST /v1/skills/drafts/{id}/reject", h.authed(h.rejectSkillDraft, auth.ActionSessionCreate))

	// ── 连接器 / 凭据托管 ───────────────────────────────────────
	mux.HandleFunc("GET /v1/connectors", h.authed(h.listConnectors, auth.ActionSessionRead))
	mux.HandleFunc("POST /v1/connectors/{name}/connect", h.authed(h.connectConnector, auth.ActionKeyManage))
	mux.HandleFunc("POST /v1/connectors/{name}/key", h.authed(h.putConnectorKey, auth.ActionKeyManage))
	mux.HandleFunc("DELETE /v1/connectors/{name}", h.authed(h.deleteConnector, auth.ActionKeyManage))
	// OAuth 回调由浏览器直接访问，没有 Authorization 头，靠 state 鉴权
	mux.HandleFunc("GET /v1/connectors/callback", h.connectorCallback)

	// ── 多 agent 协作 ───────────────────────────────────────────
	mux.HandleFunc("POST /v1/runs/{id}/children", h.authed(h.spawnChild, auth.ActionRunCreate))
	mux.HandleFunc("GET /v1/runs/{id}/children", h.authed(h.listChildren, auth.ActionSessionRead))

	// ── 行动账本（用户视角）─────────────────────────────────────
	mux.HandleFunc("GET /v1/runs/{id}/activity", h.authed(h.runActivity, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/sessions/{id}/activity", h.authed(h.sessionActivity, auth.ActionSessionRead))

	// ── 文档模块（只读，随二进制分发）────────────────────────────
	mux.HandleFunc("GET /v1/docs", h.authed(h.listDocs, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/docs/search", h.authed(h.searchDocs, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/docs/{path...}", h.authed(h.getDoc, auth.ActionSessionRead))

	// ── 自闭环：能力发现 / 技能注册 / 失败诊断 ─────────────────
	mux.HandleFunc("GET /v1/capabilities", h.authed(h.listCapabilities, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/capabilities/{name}", h.authed(h.getCapability, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/skills", h.authed(h.listSkills, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/skills/{name}", h.authed(h.getSkill, auth.ActionSessionRead))
	mux.HandleFunc("GET /v1/insights", h.authed(h.listInsights, auth.ActionAuditRead))

	// ── 浏览器 ──────────────────────────────────────────────────
	mux.HandleFunc("POST /v1/sessions/{id}/browser", h.authed(h.launchBrowser, auth.ActionRunCreate))
	mux.HandleFunc("GET /v1/sessions/{id}/browser", h.authed(h.getBrowser, auth.ActionSessionRead))
	mux.HandleFunc("DELETE /v1/sessions/{id}/browser", h.authed(h.closeBrowser, auth.ActionRunCreate))

	// 平台层路由（认证入口、管理面、沙箱、用量、契约、网关）在 platform_routes.go。
	// 单独一个文件是因为这块会持续变长，塞进这里会把「运行时」和「治理」混在一起。
	h.registerPlatform(mux)
	h.registerSetup(mux)

	// 前端静态资源。放最后：/ 是兜底路由，注册顺序不影响 ServeMux 的最长前缀匹配，
	// 但把它写在最后能让人一眼看出「其它都是 API」。
	mux.Handle("GET /", staticHandler())

	return withRecover(withCORS(mux), lg), nil
}

type handler struct {
	cfg Config
	log *log.Logger
}

// ── 认证与授权 ───────────────────────────────────────────────

// principal 取当前身份。没经过认证中间件时返回兼容模式身份（全权）。
func (h *handler) principal(r *http.Request) auth.Principal {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p
	}
	return auth.Principal{Via: auth.ViaDev}
}

// authed 包一层认证 + 授权。
//
// 三种情形必须区分清楚，否则会出现「以为上了认证结果没生效」：
//   - Auth == nil          → 兼容模式，注入 dev 身份放行（本地演示）
//   - Auth != nil, !RequireAuth → 带了凭据就解析，不带也放行（灰度期）
//   - Auth != nil, RequireAuth  → 必须带凭据，否则 401
func (h *handler) authed(fn http.HandlerFunc, act auth.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 兼容模式：整个服务没有认证器，等同于单机版。
		if h.cfg.Auth == nil {
			fn(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Via: auth.ViaDev})))
			return
		}

		token := bearerToken(r)
		if token == "" {
			if !h.cfg.RequireAuth {
				fn(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Via: auth.ViaDev})))
				return
			}
			fail(w, http.StatusUnauthorized, "需要登录")
			return
		}

		// 沙箱凭据走独立分支：白名单本身就是它的权限边界，不再过 RBAC。
		// 放在 Authenticate 之前，因为 Authenticate 会拒绝 scope 非空的 token。
		// trySandboxAuth 内部已做完「验签 → 会话存在性(吊销) → 白名单」三道检查，
		// 这里不再重复判白名单：两处各持一套判据迟早会漂，而且 invalid 的原因
		// 可能是「会话已删」或「端点不在白名单」，统一归到 401 更诚实。
		if p, isSandbox, valid := h.trySandboxAuth(r); isSandbox {
			if !valid {
				fail(w, http.StatusUnauthorized, "沙箱凭据无效、已过期，或无权访问该端点")
				return
			}
			fn(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
			return
		}

		p, err := h.cfg.Auth.Authenticate(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrExpired):
				fail(w, http.StatusUnauthorized, "凭据已过期，请重新登录")
			case errors.Is(err, auth.ErrTenantSuspended):
				fail(w, http.StatusForbidden, "租户已停用")
			default:
				fail(w, http.StatusUnauthorized, "凭据无效")
			}
			return
		}
		if act != "" {
			if err := p.Require(act); err != nil {
				fail(w, http.StatusForbidden, "权限不足："+string(act))
				return
			}
		}
		fn(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

// bearerToken 同时接受标准的 Authorization: Bearer 与 X-API-Key。
// 后者是给不方便改 header 的客户端留的口子（某些 CI 环境的 curl 封装）。
func bearerToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// canSee 判断该身份能否看到这个会话。
//
// 隔离放在这里而不是 SQL：file store 不支持按租户查，写进 SQL 会让两种 store
// 行为不一致。代价是遍历全表——当前量级（单机、几百会话）可以接受，
// 真上量之后再换成 ListSessionsByTenant 直查。
func (h *handler) canSee(p auth.Principal, s store.Session) bool {
	if p.Via == auth.ViaDev {
		return true
	}
	// 历史数据（接入认证之前建的会话）没有归属，不隐藏，否则用户会以为数据丢了。
	if s.TenantID == "" {
		return true
	}
	if s.TenantID != p.TenantID {
		return false
	}
	if p.Role == store.RoleMember {
		return s.UserID == p.UserID || s.UserID == ""
	}
	return true
}

// getOwnedSession 取会话并校验归属。
// 不属于自己时回 404 而不是 403：403 等于告诉对方「这个 ID 存在」。
func (h *handler) getOwnedSession(w http.ResponseWriter, r *http.Request) (store.Session, bool) {
	sess, err := h.cfg.Manager.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return store.Session{}, false
	}
	if !h.canSee(h.principal(r), sess) {
		fail(w, http.StatusNotFound, "会话不存在")
		return store.Session{}, false
	}
	return sess, true
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// audit 记一条审计。Auditor 为空时静默——审计不该成为主流程的失败点。
func (h *handler) audit(r *http.Request, action, targetType, targetID string, detail any) {
	if h.cfg.Auditor == nil {
		return
	}
	p := h.principal(r)
	var raw json.RawMessage
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			raw = b
		}
	}
	h.cfg.Auditor.Audit(r.Context(), store.AuditLog{
		TenantID:   p.TenantID,
		UserID:     p.UserID,
		ActorEmail: p.Email,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     raw,
		IP:         clientIP(r),
		CreatedAt:  time.Now().UTC(),
	})
}

// clientIP 取真实来源。只在可信代理后看 X-Forwarded-For，
// 否则任何人都能伪造审计里的 IP。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── 会话 ─────────────────────────────────────────────────────

func (h *handler) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
	}
	// 允许空 body：前端第一次进来就 POST 一个空对象。
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return
		}
	}
	cwd := body.Cwd
	if cwd == "" {
		cwd = h.cfg.DefaultCwd
	}
	p := h.principal(r)

	// 准入控制在起沙箱之前。放在后面等于「先占了资源再告诉你超限」，
	// 用户看到的是「创建成功后立刻被回收」，比直接拒绝难懂得多。
	if h.cfg.Quota != nil && p.TenantID != "" {
		if err := h.cfg.Quota.Check(r.Context(), p.TenantID); err != nil {
			if errors.Is(err, lifecycle.ErrQuotaExceeded) {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error": err.Error(),
					"code":  "sandbox_quota_exceeded",
				})
				return
			}
			h.failErr(w, err)
			return
		}
	}

	sess, err := h.cfg.Manager.CreateSession(r.Context(), runner.CreateSessionOptions{
		Cwd:      cwd,
		TenantID: p.TenantID,
		UserID:   p.UserID,
	})
	if err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "session.create", "session", sess.ID, nil)
	writeJSON(w, http.StatusCreated, sess)
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	sess, err := h.cfg.Manager.ListSessions(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	p := h.principal(r)
	out := make([]store.Session, 0, len(sess))
	for _, s := range sess {
		if h.canSee(p, s) {
			out = append(out, s)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// updateSession 只允许改标题。其它字段（cwd / agentSessionId / 状态）都是服务端
// 与沙箱的事实，让客户端改等于埋一个「界面显示的和实际跑的不一致」的坑。
func (h *handler) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		fail(w, http.StatusBadRequest, "title 不能为空")
		return
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80])
	}

	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	sess.Title = title
	if err := h.cfg.Store.PutSession(r.Context(), sess); err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	if err := h.cfg.Manager.DeleteSession(r.Context(), sess.ID); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "session.delete", "session", sess.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ── 审批 ─────────────────────────────────────────────────────

// listApprovals 列出某个 run 正在等人的审批。
// 前端刚打开页面时用它对齐状态：SSE 只推「连上之后」的事件。
func (h *handler) listApprovals(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedRun(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Manager.ListApprovals(r.PathValue("id")))
}

func (h *handler) resolveApproval(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedRun(w, r); !ok {
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	err := h.cfg.Manager.ResolveApproval(r.PathValue("id"), r.PathValue("toolCallId"), body.Decision)
	if err != nil {
		h.failErr(w, err)
		return
	}
	// 审批是最高危的动作（等于授权 agent 越界执行），必须留痕。
	h.audit(r, "approval.resolve", "run", r.PathValue("id"), map[string]string{
		"toolCallId": r.PathValue("toolCallId"),
		"decision":   body.Decision,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"runId":      r.PathValue("id"),
		"toolCallId": r.PathValue("toolCallId"),
		"decision":   body.Decision,
	})
}

func (h *handler) createRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Input) == "" {
		fail(w, http.StatusBadRequest, "input 不能为空")
		return
	}
	// 归属校验必须在起 run 之前：否则别人能往你的会话里塞 prompt。
	if _, ok := h.getOwnedSession(w, r); !ok {
		return
	}
	run, err := h.cfg.Manager.StartRun(r.Context(), r.PathValue("id"), body.Input)
	if err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "run.create", "run", run.ID, map[string]string{"sessionId": r.PathValue("id")})
	writeJSON(w, http.StatusCreated, run)
}

func (h *handler) listRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedSession(w, r); !ok {
		return
	}
	runs, err := h.cfg.Store.ListRuns(r.Context(), r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h *handler) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := h.getOwnedRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// getOwnedRun 按 run id 反查会话归属。
//
// 多一次查询是刻意的：run 表里没有 tenant 字段（它总是从属于会话），
// 冗余存一份就会在会话转移归属时不一致。
func (h *handler) getOwnedRun(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	run, err := h.cfg.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return store.Run{}, false
	}
	sess, err := h.cfg.Store.GetSession(r.Context(), run.SessionID)
	if err != nil {
		h.failErr(w, err)
		return store.Run{}, false
	}
	if !h.canSee(h.principal(r), sess) {
		fail(w, http.StatusNotFound, "run 不存在")
		return store.Run{}, false
	}
	return run, true
}

func (h *handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedRun(w, r); !ok {
		return
	}
	if err := h.cfg.Manager.Cancel(r.Context(), r.PathValue("id")); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "run.cancel", "run", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusAccepted)
}

// events 是 SSE。
//
// 补历史与订阅实时的先后顺序很关键：
//  1. 先 Subscribe（从「此刻」开始收）
//  2. 再读 store 补 after_seq 之后的历史
//  3. 推送时丢掉 seq <= 已推过的
//
// 反过来的话，读历史期间的实时事件会永久丢失，客户端只能看到半截流。
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	// 先确认 run 存在且属于自己，否则会挂着一个永远没有内容的连接
	// ——更糟的是会把别人的对话内容推给攻击者。
	if _, ok := h.getOwnedRun(w, r); !ok {
		return
	}

	after := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			after = n
		}
	}
	// 断线重连时浏览器会带 Last-Event-ID，等价于 after_seq。
	if v := r.Header.Get("Last-Event-ID"); v != "" && after == 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			after = n
		}
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "当前 ResponseWriter 不支持流式")
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // 长连接，别让默认超时掐掉

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache, no-transform")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no") // nginx 默认会缓冲 SSE
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 1000\n\n")
	fl.Flush()

	sub := h.cfg.Hub.Subscribe(runID)
	defer sub.Close()

	last := after
	// 回放历史。
	history, err := h.cfg.Store.Events(r.Context(), runID, after, 0)
	if err != nil {
		h.log.Printf("回放 %s 失败: %v", runID, err)
		return
	}
	terminal := false
	for _, ev := range history {
		if err := writeEvent(w, ev); err != nil {
			return
		}
		last = ev.Seq
		terminal = terminal || isTerminalEvent(ev)
	}
	fl.Flush()

	// 已经结束的 run 不需要挂连接：客户端拿到终态自己收工。
	if terminal {
		return
	}

	ticker := time.NewTicker(h.cfg.Heartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 注释帧，不产生 data，客户端不会当成事件。
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, ok := <-sub.Events():
			if !ok {
				// 缓冲满被主动断开：告诉客户端重连续传，而不是假装正常结束。
				fmt.Fprint(w, "event: overflow\ndata: {}\n\n")
				fl.Flush()
				return
			}
			if ev.Seq <= last {
				continue // 已在历史里推过
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			last = ev.Seq
			fl.Flush()
			if isTerminalEvent(ev) {
				return
			}
		}
	}
}

// history 是 events 的 JSON 版本，用于首屏补历史。
//
// 为什么不让前端直接读 SSE 接口：那个接口对已结束的 run 会「回放完就关」，
// 用 fetch 读出来是一段 text/event-stream 文本，还得在前端再解析一遍协议。
// 分成两个接口后，SSE 只负责「从此刻起」，历史用普通 JSON——两边都简单。
func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, ok := h.getOwnedRun(w, r); !ok {
		return
	}
	after := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			after = n
		}
	}
	evs, err := h.cfg.Store.Events(r.Context(), runID, after, 0)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func writeEvent(w http.ResponseWriter, ev store.Event) error {
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, payload)
	return err
}

// isTerminalEvent 判断这条事件是否宣告 run 结束。
func isTerminalEvent(ev store.Event) bool {
	if ev.Type != store.EventState {
		return false
	}
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return false
	}
	return store.IsTerminal(p.Status)
}

// ── 中间件 ───────────────────────────────────────────────────

func withRecover(next http.Handler, lg *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// 一个 panic 不该把整个控制面带下线。
				lg.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
				fail(w, http.StatusInternalServerError, fmt.Sprint(v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCORS 仅为本地开发：演示页可能从别的端口打开。
// 生产由 APISIX 统一处理，不靠这段代码。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── 响应 ─────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 命令里常有 < > &，转义了反而难读
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// failErr 把内部错误映射成状态码。存储层只暴露 ErrNotFound / ErrConflict 两种语义，
// 其余一律 500——给客户端猜状态码没有意义。
func (h *handler) failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, os.ErrNotExist):
		fail(w, http.StatusNotFound, "文件不存在: "+err.Error())
	case errors.Is(err, errOutsideRoot):
		// 403 而不是 404：这里是明确的越界尝试，不该装作「没有这个文件」。
		fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, sandbox.ErrUnsupported):
		fail(w, http.StatusNotImplemented, "当前沙箱 provider 不支持直接从宿主读文件")
	default:
		h.log.Printf("请求失败: %v", err)
		fail(w, http.StatusInternalServerError, err.Error())
	}
}
