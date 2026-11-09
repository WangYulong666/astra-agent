package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/WangYulong666/astra-agent/internal/auth"
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

// egressTenant 解析出本次请求只能操作的租户。
//
// 只认 Principal.TenantID：仓库里没有平台级角色——audit:read / tenant:manage
// 都是**租户级**权限（租户 owner/admin 都可能有），拿它放行 ?tenant= 等于让
// A 租户的 admin 读 B 租户的出网记录与策略。等真的引入平台角色（能跨租户
// 读写的独立身份）再在这里加分支，而不是复用租户角色。
//
// 传了 ?tenant= 且不等于自己时回 403 并说明原因：这里不是「资源不存在」，
// 而是「这个能力不存在」，诚实报错比 404 更好排障。
func egressTenant(w http.ResponseWriter, p auth.Principal, r *http.Request) (string, bool) {
	if tid := r.URL.Query().Get("tenant"); tid != "" && tid != p.TenantID {
		fail(w, http.StatusForbidden, "没有平台级角色，不支持跨租户查询/修改出网配置；请去掉 tenant 参数（只能操作自己租户）")
		return "", false
	}
	return p.TenantID, true
}

func (h *handler) egressPolicy(w http.ResponseWriter, r *http.Request) {
	if h.egressDisabled(w) {
		return
	}
	tid, ok := egressTenant(w, h.principal(r), r)
	if !ok {
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
	tid, ok := egressTenant(w, p, r)
	if !ok {
		return
	}
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
	if err := h.egressOf().SetPolicy(r.Context(), tid, pol); err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (h *handler) egressLog(w http.ResponseWriter, r *http.Request) {
	if h.egressDisabled(w) {
		return
	}
	tid, ok := egressTenant(w, h.principal(r), r)
	if !ok {
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 500)
		}
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
