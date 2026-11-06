package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/connector"
)

func (h *handler) connectorsDisabled(w http.ResponseWriter) bool {
	if h.cfg.Connectors == nil || !h.cfg.Connectors.Enabled() {
		fail(w, http.StatusServiceUnavailable, "连接器未启用：请配置 ASTRA_VAULT_KEY（32 字节 base64）后重启")
		return true
	}
	return false
}

// listConnectors 返回内置连接器 + 当前用户已连接的状态。
// 刻意不回 token：连"前缀"都不回，凭据只能被使用，不能被读取。
func (h *handler) listConnectors(w http.ResponseWriter, r *http.Request) {
	if h.connectorsDisabled(w) {
		return
	}
	p := h.principal(r)
	all := h.cfg.Connectors.List()
	linked, err := h.cfg.Connectors.ListCredentials(r.Context(), p.TenantID, p.UserID)
	if err != nil && !errors.Is(err, connector.ErrDisabled) {
		h.failErr(w, err)
		return
	}
	byName := map[string]connector.Credential{}
	for _, c := range linked {
		byName[c.Name] = c
	}
	type item struct {
		connector.Connector
		Connected bool   `json:"connected"`
		LinkedAt  string `json:"linked_at,omitempty"`
		Expires   string `json:"expires,omitempty"`
	}
	out := make([]item, 0, len(all))
	for _, c := range all {
		it := item{Connector: c}
		if cred, ok := byName[c.Name]; ok {
			it.Connected = true
			it.LinkedAt = cred.LinkedAt.Format("2006-01-02T15:04:05Z07:00")
			if !cred.Expires.IsZero() {
				it.Expires = cred.Expires.Format("2006-01-02T15:04:05Z07:00")
			}
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

// connectConnector 生成 OAuth 授权地址；APIKey 型返回 needs_key=true。
func (h *handler) connectConnector(w http.ResponseWriter, r *http.Request) {
	if h.connectorsDisabled(w) {
		return
	}
	p := h.principal(r)
	name := r.PathValue("name")
	redirect := h.cfg.ConnectorRedirectURL
	url, err := h.cfg.Connectors.AuthorizeURL(r.Context(), p.TenantID, p.UserID, name, redirect, "")
	if err != nil {
		h.connectorFail(w, err)
		return
	}
	if url == "" {
		writeJSON(w, http.StatusOK, map[string]any{"needs_key": true, "name": name})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": url})
}

// connectorCallback 是 OAuth 回调：**不需要登录**，靠 state 里的签名鉴权。
// 浏览器带着 code 回来时没有 Authorization 头，这是标准流程。
func (h *handler) connectorCallback(w http.ResponseWriter, r *http.Request) {
	if h.connectorsDisabled(w) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		fail(w, http.StatusBadRequest, "缺少 code 或 state")
		return
	}
	tenantID, userID, name, err := h.cfg.Connectors.ParseState(state)
	if err != nil {
		fail(w, http.StatusBadRequest, "state 无效或已过期，请重新发起授权")
		return
	}
	cred, err := h.cfg.Connectors.Exchange(r.Context(), tenantID, userID, name, code, h.cfg.ConnectorRedirectURL)
	if err != nil {
		h.connectorFail(w, err)
		return
	}
	// 回调是浏览器直达的，直接回一页 HTML 比回 JSON 友好
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>已连接</title>" +
		"<body style=\"font-family:-apple-system,sans-serif;padding:48px;text-align:center\">" +
		"<h2 style=\"font-weight:500\">已连接 " + cred.Name + "</h2>" +
		"<p style=\"color:#666\">可以关掉这个页面，回到终端或控制台。</p></body>"))
}

// putConnectorKey 存一个 API key（很多国内 SaaS 没有 OAuth）。
func (h *handler) putConnectorKey(w http.ResponseWriter, r *http.Request) {
	if h.connectorsDisabled(w) {
		return
	}
	p := h.principal(r)
	var req struct {
		Key string `json:"key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Key == "" {
		fail(w, http.StatusBadRequest, "key 不能为空")
		return
	}
	name := r.PathValue("name")
	if err := h.cfg.Connectors.PutKey(r.Context(), p.TenantID, p.UserID, name, req.Key); err != nil {
		h.connectorFail(w, err)
		return
	}
	// 审计记「谁在什么时候连了什么」，不记 key 本身
	h.audit(r, "connector.key", "connector", name, nil)
	writeJSON(w, http.StatusOK, map[string]any{"connected": true, "name": name})
}

func (h *handler) deleteConnector(w http.ResponseWriter, r *http.Request) {
	if h.connectorsDisabled(w) {
		return
	}
	p := h.principal(r)
	name := r.PathValue("name")
	if err := h.cfg.Connectors.Delete(r.Context(), p.TenantID, p.UserID, name); err != nil {
		h.connectorFail(w, err)
		return
	}
	h.audit(r, "connector.delete", "connector", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) connectorFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connector.ErrNotFound), errors.Is(err, connector.ErrNotConnected):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, connector.ErrUnsupported):
		fail(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, connector.ErrDisabled):
		fail(w, http.StatusServiceUnavailable, err.Error())
	default:
		h.failErr(w, err)
	}
}
