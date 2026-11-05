package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/memory"
)

// memoryStore 返回记忆存储。未配置时返回 Disabled（不是 nil），
// 让上层统一处理「能力关着」而不是每处判空。
func (h *handler) memoryStore() memory.Store {
	if h.cfg.Memory == nil {
		return memory.Disabled()
	}
	return h.cfg.Memory
}

// memoryFail 把「未启用」映射成 503 + 可操作的原因，其余交给通用错误映射。
func (h *handler) memoryFail(w http.ResponseWriter, err error) {
	if errors.Is(err, memory.ErrDisabled) {
		fail(w, http.StatusServiceUnavailable, "记忆能力未启用：请配置 MILVUS_ADDR 后重启")
		return
	}
	h.failErr(w, err)
}

func (h *handler) memoryIngest(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	if p.UserID == "" {
		fail(w, http.StatusUnauthorized, "写入记忆需要登录身份")
		return
	}
	var req struct {
		Kind      string         `json:"kind"`
		Text      string         `json:"text"`
		SessionID string         `json:"session_id"`
		Meta      map[string]any `json:"meta"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		fail(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = memory.KindFact
	}
	if kind != memory.KindFact && kind != memory.KindConversation {
		fail(w, http.StatusBadRequest, "kind 只能是 fact 或 conversation")
		return
	}

	// tenant / user 一律取自身份，不接受请求体传入——否则等于允许往别人名下写。
	it, err := h.memoryStore().Ingest(r.Context(), memory.Item{
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		SessionID: req.SessionID,
		Kind:      kind,
		Text:      text,
		Meta:      req.Meta,
	})
	if err != nil {
		h.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

func (h *handler) memorySearch(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	if p.UserID == "" {
		fail(w, http.StatusUnauthorized, "检索记忆需要登录身份")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		fail(w, http.StatusBadRequest, "缺少 q 参数")
		return
	}
	k := 10
	if v := r.URL.Query().Get("k"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, "k 必须是正整数")
			return
		}
		k = min(n, 50) // 上限 50：再多也不适合直接塞进 prompt
	}
	items, err := h.memoryStore().Search(r.Context(), memory.Query{
		TenantID: p.TenantID,
		UserID:   p.UserID,
		Text:     q,
		K:        k,
		Kind:     r.URL.Query().Get("kind"),
	})
	if err != nil {
		h.memoryFail(w, err)
		return
	}
	if items == nil {
		items = []memory.Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "query": q, "k": k})
}

func (h *handler) memoryDelete(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	if p.UserID == "" {
		fail(w, http.StatusUnauthorized, "删除记忆需要登录身份")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		fail(w, http.StatusBadRequest, "缺少记忆 id")
		return
	}
	// tenant + user 一起进过滤条件：别人的 id 即便被猜到也删不掉。
	if err := h.memoryStore().Delete(r.Context(), p.TenantID, p.UserID, id); err != nil {
		h.memoryFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) memoryStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.memoryStore().Stats(r.Context())
	if err != nil {
		h.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
