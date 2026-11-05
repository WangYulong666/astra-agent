package api

// SSE 票据：让原生 EventSource 能在强制认证下连上事件流。
//
// 背景：GET /v1/runs/{id}/events 原本只认 Authorization: Bearer，而浏览器的
// EventSource 不能设置自定义 header，前端被迫用 fetch + ReadableStream 手写
// SSE 解析，断线重连、Last-Event-ID 这些原生能力全部作废。
//
// 解法是短时一次性票据：登录态 POST /v1/auth/sse-ticket 换一张 60 秒的 JWT
// （scope=sse、jti 用后即焚），EventSource 用 ?ticket= 打开连接。票据只授予
// 这一个端点的只读权限——它进普通认证路径（Authenticate）会被当场拒绝，
// 见 auth.Service.RedeemSSETicket 的注释。
//
// 已知代价：票据出现在 URL 里，会落进访问日志与浏览器历史。60 秒 TTL +
// 一次性消费把影响压到可接受；这是 EventSource 限制下的行业通解，不是漏洞。

import (
	"errors"
	"net/http"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/auth"
)

// authedEvents 是 events 端点的认证包装：带 Authorization 走标准 authed，
// 不带则尝试 ?ticket= 票据。两条路的授权动作完全一致（act），票据只解决
// 「凭据怎么递过来」，不放宽「能做什么」。
func (h *handler) authedEvents(fn http.HandlerFunc, act auth.Action) http.HandlerFunc {
	authedFn := h.authed(fn, act)
	return func(w http.ResponseWriter, r *http.Request) {
		// 带了常规凭据（fetch 流程、curl、脚本）就走老路，行为完全不变。
		if bearerToken(r) != "" {
			authedFn(w, r)
			return
		}

		// 以下对应 authed 里「无凭据」的三种情形，语义必须一致：
		// 兼容模式全放行；灰度期不带凭据也放行；强制认证才验票据。
		if h.cfg.Auth == nil || !h.cfg.RequireAuth {
			fn(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Via: auth.ViaDev})))
			return
		}

		ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
		if ticket == "" {
			fail(w, http.StatusUnauthorized, "需要登录或 SSE 票据")
			return
		}
		p, err := h.redeemSSETicket(r, ticket)
		if err != nil {
			h.failSSEAuth(w, err)
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

// redeemSSETicket 消费一张票据。只有具体类型 auth.Service 有这能力
// （与 issueLLMToken 的断言同款理由：接口保持最小）。
func (h *handler) redeemSSETicket(r *http.Request, ticket string) (auth.Principal, error) {
	svc, ok := h.cfg.Auth.(*auth.Service)
	if !ok {
		return auth.Principal{}, auth.ErrInvalidCredential
	}
	return svc.RedeemSSETicket(r.Context(), ticket)
}

// failSSEAuth 与 authed 的错误映射保持同一种口径：过期与无效都回 401，
// 不给探测者更多信息。
func (h *handler) failSSEAuth(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrExpired):
		fail(w, http.StatusUnauthorized, "SSE 票据已过期，请重新获取")
	default:
		fail(w, http.StatusUnauthorized, "SSE 票据无效")
	}
}

// sseTicket 签发一次性事件流票据。
//
// 任何已登录用户都可签发（act 为空）：票据本身不授权任何会话，
// 真正的归属校验发生在 events 的 getOwnedRun——拿别人的票据连自己的 run、
// 拿自己的票据连别人的 run，都过不了那一关。
func (h *handler) sseTicket(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Auth == nil {
		fail(w, http.StatusNotImplemented, "认证未启用（兼容模式直接连接即可）")
		return
	}
	svc, ok := h.cfg.Auth.(*auth.Service)
	if !ok {
		fail(w, http.StatusNotImplemented, "当前认证实现不支持签发 SSE 票据（需要启用 auth.Service）")
		return
	}
	p := h.principal(r)
	if p.UserID == "" || p.TenantID == "" {
		fail(w, http.StatusBadRequest, "当前为兼容模式，没有可签发票据的用户身份")
		return
	}
	ticket, exp, err := svc.IssueSSETicket(p.UserID, p.TenantID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    ticket,
		"expiresAt": exp,
	})
}
