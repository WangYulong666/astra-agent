package api

import (
	"net/http"
	"strconv"
	"strings"

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
//
// 必须带 run_id：不带时 Langfuse 会返回全项目的 trace 列表，而 Summary.Input
// 是用户原始 prompt。当前上报没有打租户 tag（Langfuse 侧无法按租户过滤，
// 做 tag 级过滤的成本远高于收益），所以只暴露「按 run 精确查询」这一种用法：
// 先走 getOwnedRun 校验归属，再拿这**一条** trace。
func (h *handler) obsTraces(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Trace == nil || !h.cfg.Trace.Enabled() {
		fail(w, http.StatusServiceUnavailable, "观测能力未启用：请配置 LANGFUSE_HOST 等变量后重启")
		return
	}
	runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
	if runID == "" {
		fail(w, http.StatusBadRequest, "为避免跨租户泄露，必须指定 run_id（trace 里含用户原始 prompt）")
		return
	}
	// getOwnedRun 从 PathValue 取 run id；这里把 query 参数注入路径值以复用
	// 同一套归属判断（不通过回 404，不告诉对方这个 run 是否存在）。
	r.SetPathValue("id", runID)
	if _, ok := h.getOwnedRun(w, r); !ok {
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
		RunID: runID,
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
