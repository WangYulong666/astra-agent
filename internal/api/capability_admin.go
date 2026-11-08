package api

import (
	"net/http"
	"strconv"
)

// 能力发现：渐进式披露三层。
//
//	GET /v1/capabilities              只给 name/kind/description/enabled（常驻层）
//	GET /v1/capabilities?detail=full   加 endpoint/requiredEnv/requires（命中层）
//	GET /v1/capabilities/{name}        单项全文，含 SKILL.md 正文（资源层）
func (h *handler) listCapabilities(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Capabilities == nil {
		fail(w, http.StatusServiceUnavailable, "能力注册表未启用")
		return
	}
	detail := r.URL.Query().Get("detail") == "full"
	items, err := h.cfg.Capabilities.List(r.Context(), detail)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(items), "detail": detail})
}

func (h *handler) getCapability(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Capabilities == nil {
		fail(w, http.StatusServiceUnavailable, "能力注册表未启用")
		return
	}
	item, err := h.cfg.Capabilities.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// GET /v1/skills 列出已注册技能（内置 + distill 产物），不再是前端硬编码的 3 个。
func (h *handler) listSkills(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SkillHub == nil {
		fail(w, http.StatusServiceUnavailable, "技能注册表未启用")
		return
	}
	items, err := h.cfg.SkillHub.List(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(items)})
}

func (h *handler) getSkill(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SkillHub == nil {
		fail(w, http.StatusServiceUnavailable, "技能注册表未启用")
		return
	}
	s, err := h.cfg.SkillHub.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func capabilityLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 50
	}
	if n > 500 {
		return 500
	}
	return n
}
