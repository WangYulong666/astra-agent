package api

import (
	"net/http"
	"strconv"

	"github.com/WangYulong666/astra-agent/internal/trace"
)

// obsConfig 给运营页/体检脚本看「观测能力到底开没开、上报到哪」。
// 只暴露 public key：secret key 留在服务端，绝不进响应。
func (h *handler) obsConfig(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"enabled":       false,
		"endpoint":      "",
		"dashboard_url": "",
		"public_key":    "",
	}
	if h.cfg.Trace != nil && h.cfg.Trace.Enabled() {
		out["enabled"] = true
		out["endpoint"] = h.cfg.TraceEndpoint
		out["dashboard_url"] = h.cfg.TraceDashboardURL
		out["public_key"] = h.cfg.TracePublicKey
	}
	writeJSON(w, http.StatusOK, out)
}

// obsTraces 代理查询 Langfuse，避免把 secret key 下发到浏览器。
func (h *handler) obsTraces(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Trace == nil || !h.cfg.Trace.Enabled() {
		fail(w, http.StatusServiceUnavailable, "观测能力未启用：请配置 LANGFUSE_HOST 等变量后重启")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, "limit 必须是正整数")
			return
		}
		limit = min(n, 100)
	}
	items, err := h.cfg.Trace.QueryTraces(r.Context(), trace.Filter{
		RunID: r.URL.Query().Get("run_id"),
		Limit: limit,
	})
	if err != nil {
		h.failErr(w, err)
		return
	}
	if items == nil {
		items = []trace.Summary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit})
}
