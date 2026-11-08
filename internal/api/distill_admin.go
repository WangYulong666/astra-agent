package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/distill"
)

func (h *handler) distillDisabled(w http.ResponseWriter) bool {
	if h.cfg.Distiller == nil || !h.cfg.Distiller.Enabled() {
		fail(w, http.StatusServiceUnavailable, "技能沉淀未启用：请以 -distill 启动后重启")
		return true
	}
	return false
}

// distillRun 从一次已完成的 run 还原技能草稿。
func (h *handler) distillRun(w http.ResponseWriter, r *http.Request) {
	if h.distillDisabled(w) {
		return
	}
	run, ok := h.getOwnedRun(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)

	p := h.principal(r)
	d, err := h.cfg.Distiller.Distill(r.Context(), distill.Request{
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		RunID:     run.ID,
		SessionID: run.SessionID,
		Name:      req.Name,
	})
	if err != nil {
		h.distillFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *handler) listSkillDrafts(w http.ResponseWriter, r *http.Request) {
	if h.distillDisabled(w) {
		return
	}
	p := h.principal(r)
	items, err := h.cfg.Distiller.List(r.Context(), p.TenantID)
	if err != nil {
		h.distillFail(w, err)
		return
	}
	if !p.Can("user:manage") {
		kept := items[:0]
		for _, d := range items {
			if d.UserID == p.UserID {
				kept = append(kept, d)
			}
		}
		items = kept
	}
	if items == nil {
		items = []distill.Draft{}
	}
	writeJSON(w, http.StatusOK, items)
}

// acceptSkillDraft 把草稿写成 skill 目录，返回落地路径。
func (h *handler) acceptSkillDraft(w http.ResponseWriter, r *http.Request) {
	if h.distillDisabled(w) {
		return
	}
	p := h.principal(r)
	path, err := h.cfg.Distiller.Accept(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		h.distillFail(w, err)
		return
	}
	// 立刻刷新技能索引：skillhub.List 本身也会懒刷新，但 accept 的当次请求
	// 就该看到新技能，不该等下一次查询。
	if h.cfg.SkillHub != nil {
		_ = h.cfg.SkillHub.Refresh(r.Context())
	}
	h.audit(r, "skill.accept", "draft", r.PathValue("id"), map[string]any{"path": path})
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func (h *handler) rejectSkillDraft(w http.ResponseWriter, r *http.Request) {
	if h.distillDisabled(w) {
		return
	}
	p := h.principal(r)
	if err := h.cfg.Distiller.Reject(r.Context(), p.TenantID, r.PathValue("id")); err != nil {
		h.distillFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) distillFail(w http.ResponseWriter, err error) {
	if errors.Is(err, distill.ErrNotFound) {
		fail(w, http.StatusNotFound, "草稿不存在")
		return
	}
	if errors.Is(err, distill.ErrDisabled) {
		fail(w, http.StatusServiceUnavailable, "技能沉淀未启用")
		return
	}
	h.failErr(w, err)
}
