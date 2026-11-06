package api

import (
	"net/http"
	"strconv"
)

// 行动账本与 audit log 的区别：它按用户视角聚合「做了什么、卡在哪」，
// 只读、不含分页游标，limit 默认 200、上限 1000（一次看不完说明该分页了）。
func (h *handler) runActivity(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ledger == nil {
		fail(w, http.StatusServiceUnavailable, "行动账本未启用")
		return
	}
	run, ok := h.getOwnedRun(w, r)
	if !ok {
		return
	}
	limit := activityLimit(r)
	entries, err := h.cfg.Ledger.ForRun(r.Context(), run.ID, limit)
	if err != nil {
		h.failErr(w, err)
		return
	}
	summary, err := h.cfg.Ledger.Summary(r.Context(), run.ID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runId": run.ID, "entries": orEmpty(entries), "summary": summary,
	})
}

func (h *handler) sessionActivity(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ledger == nil {
		fail(w, http.StatusServiceUnavailable, "行动账本未启用")
		return
	}
	id := r.PathValue("id")
	sess, err := h.cfg.Store.GetSession(r.Context(), id)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if !h.canSee(h.principal(r), sess) {
		fail(w, http.StatusNotFound, "会话不存在")
		return
	}
	entries, err := h.cfg.Ledger.ForSession(r.Context(), id, activityLimit(r))
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": id, "entries": orEmpty(entries),
	})
}

func activityLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 200
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// orEmpty 保证 JSON 里是 [] 而不是 null（前端 .map 不必到处判空）。
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
