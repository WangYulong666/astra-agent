package api

import (
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/insight"
)

// GET /v1/insights 列出失败诊断。
//
// 隔离规则：
//   - 带 session_id：先查会话并过 canSee，不通过回 404（403 等于承认这个会话存在）。
//   - 不带：只返回本租户（ByTenant），绝不调 All。诊断里含错误文本、最后工具名
//     和可能带路径/命令的建议，跨租户泄露的代价远高于「运维要换个姿势查」。
//     仓库里没有平台级角色，所以这里没有「看全部租户」的分支。
//
// 老数据（接入认证前写入、没有 tenantId/userId）只在单机兼容模式（ViaDev）下
// 可见——那条路径走 All。正常身份用 ByTenant(p.TenantID)，空 TenantID 天然
// 匹配不到它们。取舍写在 insight.Store.ByTenant 的注释里：没有归属的数据
// 猜不出属于谁，宁可少展示，也不猜一个租户。
func (h *handler) listInsights(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Insights == nil {
		fail(w, http.StatusServiceUnavailable, "诊断能力未启用")
		return
	}
	p := h.principal(r)
	limit := capabilityLimit(r)

	var (
		items []insight.Diagnosis
		err   error
	)
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		if h.cfg.Store == nil {
			// 没有存储就无法校验归属；失败关闭，不返回任何诊断。
			fail(w, http.StatusNotFound, "会话不存在")
			return
		}
		sess, sErr := h.cfg.Store.GetSession(r.Context(), sid)
		if sErr != nil || !h.canSee(p, sess) {
			fail(w, http.StatusNotFound, "会话不存在")
			return
		}
		items, err = h.cfg.Insights.Recent(r.Context(), sid, limit)
	} else if p.Via == auth.ViaDev {
		// 单机兼容模式：进程里没有租户概念，全量就是「自己的全部」。
		// 老数据（无租户）也只在这条路径可见。
		items, err = h.cfg.Insights.All(r.Context(), limit)
	} else if p.TenantID == "" {
		// 有身份却没有租户：不该发生，但一旦发生必须失败关闭，
		// 否则 ByTenant("") 会正好把无归属的老数据放出来。
		items = []insight.Diagnosis{}
	} else {
		items, err = h.cfg.Insights.ByTenant(r.Context(), p.TenantID, limit)
	}
	if err != nil {
		h.failErr(w, err)
		return
	}
	if items == nil {
		items = []insight.Diagnosis{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit})
}
