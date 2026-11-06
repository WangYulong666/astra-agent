package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/WangYulong666/astra-agent/internal/egress"
)

// egressOf 取出网控制器，nil 视为未启用。
func (h *handler) egressOf() egress.Controller {
	return h.cfg.Egress
}

func (h *handler) egressDisabled(w http.ResponseWriter) bool {
	if h.egressOf() == nil || !h.egressOf().Enabled() {
		fail(w, http.StatusServiceUnavailable, "出网管控未启用：请配置 EGRESS_LISTEN_ADDR 后重启")
		return true
	}
	return false
}

func (h *handler) egressPolicy(w http.ResponseWriter, r *http.Request) {
	if h.egressDisabled(w) {
		return
	}
	p := h.principal(r)
	// 运营视角可指定租户；普通用户只能看自己的
	tid := r.URL.Query().Get("tenant")
	if tid == "" {
		tid = p.TenantID
	}
	if tid != p.TenantID && !p.Can("tenant:manage") {
		fail(w, http.StatusForbidden, "只能查看自己租户的出网策略")
		return
	}
	pol, err := h.egressOf().Policy(r.Context(), tid)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (h *handler) putEgressPolicy(w http.ResponseWriter, r *http.Request) {
	if h.egressDisabled(w) {
		return
	}
	p := h.principal(r)
	var pol egress.Policy
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(r.Body).Decode(&pol); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if !validDecision(pol.Default) {
		fail(w, http.StatusBadRequest, "default 只能是 allow / deny / ask")
		return
	}
	for _, rule := range pol.Rules {
		if rule.Pattern == "" {
			fail(w, http.StatusBadRequest, "规则缺少 pattern")
			return
		}
		if !validDecision(rule.Decision) {
			fail(w, http.StatusBadRequest, "规则 "+rule.Pattern+" 的 decision 非法")
			return
		}
	}
	if err := h.egressOf().SetPolicy(r.Context(), p.TenantID, pol); err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (h *handler) egressLog(w http.ResponseWriter, r *http.Request) {
	if h.egressDisabled(w) {
		return
	}
	p := h.principal(r)
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	// 只有能看全租户的角色才允许不带 tenant 参数看全部
	tid := r.URL.Query().Get("tenant")
	if tid == "" {
		tid = p.TenantID
	}
	if tid != p.TenantID && !p.Can("audit:read") {
		fail(w, http.StatusForbidden, "只能查看自己租户的出网记录")
		return
	}
	items, err := h.egressOf().Log(r.Context(), tid, limit)
	if err != nil && !errors.Is(err, egress.ErrDisabled) {
		h.failErr(w, err)
		return
	}
	if items == nil {
		items = []egress.LogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit})
}

func validDecision(d egress.Decision) bool {
	switch d {
	case egress.Allow, egress.Deny, egress.Ask:
		return true
	}
	return false
}
