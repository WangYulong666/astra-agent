package api

import (
	"net/http"
	"strconv"
)

// 文档模块是只读的：给用户看使用指南，也给我们看运行时生成的功能总览。
// 两者都不含租户私有数据，所以按 session:read 放行即可。
func (h *handler) listDocs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Docs == nil {
		fail(w, http.StatusServiceUnavailable, "文档模块未启用")
		return
	}
	items, err := h.cfg.Docs.Index(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(items)})
}

// GET /v1/docs/{path...}：path 允许带斜杠（design/xxx.md）。
func (h *handler) getDoc(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Docs == nil {
		fail(w, http.StatusServiceUnavailable, "文档模块未启用")
		return
	}
	doc, err := h.cfg.Docs.Get(r.Context(), r.PathValue("path"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (h *handler) searchDocs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Docs == nil {
		fail(w, http.StatusServiceUnavailable, "文档模块未启用")
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		fail(w, http.StatusBadRequest, "q 不能为空")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	items, err := h.cfg.Docs.Search(r.Context(), q, limit)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(items), "query": q})
}
