package api

import "net/http"

// GET /v1/insights 列出失败诊断。session_id 为空时给全租户（需要 owner/admin）。
func (h *handler) listInsights(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Insights == nil {
		fail(w, http.StatusServiceUnavailable, "诊断能力未启用")
		return
	}
	limit := capabilityLimit(r)
	var (
		items any
		err   error
	)
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		items, err = h.cfg.Insights.Recent(r.Context(), sid, limit)
	} else {
		items, err = h.cfg.Insights.All(r.Context(), limit)
	}
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit})
}
