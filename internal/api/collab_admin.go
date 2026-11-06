package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/collab"
)

// 子 agent 的归属一律从父 run 的会话继承，请求体里只允许给 input/title。
// 让用户传 tenantId/userId 等于允许往别人租户名下起沙箱。
func (h *handler) spawnChild(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Collab == nil {
		fail(w, http.StatusServiceUnavailable, "多 agent 协作未启用")
		return
	}
	parent, ok := h.getOwnedRun(w, r)
	if !ok {
		return
	}
	var body struct {
		Input string `json:"input"`
		Title string `json:"title"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if body.Input == "" {
		fail(w, http.StatusBadRequest, "input 不能为空")
		return
	}
	sess, err := h.cfg.Store.GetSession(r.Context(), parent.SessionID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	child, err := h.cfg.Collab.Spawn(r.Context(), collab.SpawnRequest{
		ParentRunID: parent.ID,
		Input:       body.Input,
		Title:       body.Title,
		TenantID:    sess.TenantID,
		UserID:      sess.UserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, collab.ErrTooDeep):
			fail(w, http.StatusConflict, "派生深度超限")
		case errors.Is(err, collab.ErrTimeout):
			fail(w, http.StatusGatewayTimeout, "子任务超时")
		default:
			h.failErr(w, err)
		}
		return
	}
	h.audit(r, "collab.spawn", "run", parent.ID, map[string]any{
		"childRunId": child.RunID, "status": child.Status,
	})
	writeJSON(w, http.StatusCreated, child)
}

func (h *handler) listChildren(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Collab == nil {
		fail(w, http.StatusServiceUnavailable, "多 agent 协作未启用")
		return
	}
	parent, ok := h.getOwnedRun(w, r)
	if !ok {
		return
	}
	items, err := h.cfg.Collab.List(r.Context(), parent.ID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if items == nil {
		items = []collab.Child{}
	}
	writeJSON(w, http.StatusOK, items)
}
