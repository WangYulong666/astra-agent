package api

// 平台层路由注册。与 api.go 的分界：
//   api.go            —— 运行时（会话、run、事件流），M1 就有的东西
//   platform_routes   —— 治理（身份、管理面、沙箱、用量、契约、网关）
//
// 分成两个文件不是为了好看，是因为治理层还会长（配额、计费细则、Webhook），
// 混在 api.go 里会让「一次请求的生命周期」和「一个租户的生命周期」互相污染。

import (
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/auth"
)

func (h *handler) registerPlatform(mux *http.ServeMux) {
	// ── 公开：健康与初始化 ──
	// /healthz 在 api.go（它属于「进程活着吗」，与治理无关）。
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("POST /v1/auth/bootstrap", h.bootstrap)
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("GET /metrics", h.metricsHandler)
	mux.HandleFunc("GET /docs", h.docs)
	mux.HandleFunc("GET /v1/openapi.json", h.openapiJSON)

	// ── 身份 ──
	mux.HandleFunc("POST /v1/auth/logout", h.authed(h.logout, ""))
	mux.HandleFunc("GET /v1/auth/me", h.authed(h.me, ""))
	mux.HandleFunc("PATCH /v1/auth/password", h.authed(h.changePassword, ""))
	// refresh 是公开路由：refresh token 本身就是凭据。
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	// SSE 票据：需要登录态（票据只授予事件流这一个端点的只读权限）。
	mux.HandleFunc("POST /v1/auth/sse-ticket", h.authed(h.sseTicket, ""))

	// ── 沙箱管理面 ──
	mux.HandleFunc("GET /v1/sandboxes", h.authed(h.listSandboxes, auth.ActionSandboxRead))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", h.authed(h.deleteSandbox, auth.ActionSandboxManage))

	// ── 用量与计费 ──
	mux.HandleFunc("GET /v1/usage", h.authed(h.listUsage, auth.ActionUsageRead))
	mux.HandleFunc("GET /v1/usage/billing", h.authed(h.billing, auth.ActionUsageRead))

	// ── 租户 ──
	mux.HandleFunc("GET /v1/admin/tenants", h.authed(h.listTenants, auth.ActionTenantManage))
	mux.HandleFunc("POST /v1/admin/tenants", h.authed(h.createTenant, auth.ActionTenantManage))
	mux.HandleFunc("GET /v1/admin/tenants/{id}", h.authed(h.getTenant, auth.ActionTenantManage))
	mux.HandleFunc("PATCH /v1/admin/tenants/{id}", h.authed(h.updateTenant, auth.ActionTenantManage))

	// ── 用户 ──
	mux.HandleFunc("GET /v1/admin/users", h.authed(h.listUsers, auth.ActionUserManage))
	mux.HandleFunc("POST /v1/admin/users", h.authed(h.createUser, auth.ActionUserManage))
	mux.HandleFunc("PATCH /v1/admin/users/{id}", h.authed(h.updateUser, auth.ActionUserManage))

	// ── 凭据 ──
	mux.HandleFunc("GET /v1/admin/apikeys", h.authed(h.listAPIKeys, auth.ActionKeyManage))
	mux.HandleFunc("POST /v1/admin/apikeys", h.authed(h.createAPIKey, auth.ActionKeyManage))
	mux.HandleFunc("DELETE /v1/admin/apikeys/{id}", h.authed(h.revokeAPIKey, auth.ActionKeyManage))

	// ── 审计 ──
	mux.HandleFunc("GET /v1/admin/audit", h.authed(h.listAudit, auth.ActionAuditRead))

	// ── 全租户用量（管理员视角）──
	mux.HandleFunc("GET /v1/admin/usage", h.authed(h.adminUsage, auth.ActionUsageReadAll))

	// ── LLM 网关 ──
	// 签发虚拟 key：会话维度，短期有效，真 key 不进沙箱（见设计文档 D4）。
	mux.HandleFunc("POST /v1/sessions/{id}/llm-token", h.authed(h.issueLLMToken, auth.ActionRunCreate))
	// 网关本体。用虚拟 key 认证，不走 authed（那条路径认的是平台 JWT）。
	//
	// 为什么要显式列出三个方法而不是不写方法：Go 1.22 的 ServeMux 里，
	// 「不限方法的 /v1/llm/...」与 SPA 的 `GET /` 在同一路径集合上会 panic
	// （前者方法更宽、路径更具体，两者无法比较优先级）。列出方法即可共存。
	//
	// 为什么要有 GET：agent 的 SDK 会先探 `/v1/models`（GET，无请求体）再发消息，
	// 只注册 POST 会让模型发现 405，表现成「模型列表是空的」这种莫名其妙的降级。
	// DELETE 用于 Files API 的清理，透传即可，不需要单独逻辑。
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(m+" /v1/llm/{path...}", h.llmProxy)
	}

	// ── 沙箱生命周期治理 ──
	// 与 /v1/sandboxes 的分界：那个回答「现在有哪些」，这个回答「按什么规则回收、上一轮回收了什么」。
	mux.HandleFunc("GET /v1/admin/lifecycle", h.authed(LifecycleStatus(h.cfg.Reaper), auth.ActionSandboxManage))
	mux.HandleFunc("POST /v1/admin/lifecycle/sweep", h.authed(LifecycleSweep(h.cfg.Reaper), auth.ActionSandboxManage))
}

// todoMsg 给未实现的 handler 用。放在这里而不是各 handler 文件，
// 是为了让子任务重写某个文件时不会顺手把这个共享常量删掉。
const todoMsg = "尚未实现"

// requireAdminStore 治理接口未启用时统一回 501。
//
// 为什么不直接 panic：本地可能只跑运行时（没接 SQL store），
// 前端探测管理接口是常态，回 501 比让整个进程挂掉合理。
func (h *handler) requireAdminStore(w http.ResponseWriter) bool {
	if h.cfg.Admin == nil {
		fail(w, http.StatusNotImplemented, "治理接口未启用（需要 -store sqlite 或 postgres）")
		return false
	}
	return true
}
