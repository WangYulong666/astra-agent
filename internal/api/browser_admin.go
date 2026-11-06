package api

import (
	"errors"
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/browser"
)

func (h *handler) browserOf() browser.Manager { return h.cfg.Browsers }

func (h *handler) browserDisabled(w http.ResponseWriter) bool {
	if h.browserOf() == nil || !h.browserOf().Enabled() {
		fail(w, http.StatusServiceUnavailable, "浏览器未启用：本机未找到 Chrome（配置 CHROME_PATH 或安装 Chrome）")
		return true
	}
	return false
}

// launchBrowser 给会话起一个浏览器，返回 agent 要连的 CDP 地址。
//
// 一个会话复用一个实例：agent 在多轮里要保持登录态和页面状态，
// 每轮新起一个等于每次重新登录。
func (h *handler) launchBrowser(w http.ResponseWriter, r *http.Request) {
	if h.browserDisabled(w) {
		return
	}
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	p := h.principal(r)
	// 已有实例直接复用
	for _, s := range h.browserOf().List() {
		if s.SessionID == sess.ID && s.TenantID == p.TenantID {
			writeJSON(w, http.StatusOK, s)
			return
		}
	}
	out, err := h.browserOf().Launch(r.Context(), browser.LaunchRequest{
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		SessionID: sess.ID,
	})
	if err != nil {
		h.browserFail(w, err)
		return
	}
	h.audit(r, "browser.launch", "session", sess.ID, map[string]any{"browser": out.ID})
	writeJSON(w, http.StatusCreated, out)
}

func (h *handler) getBrowser(w http.ResponseWriter, r *http.Request) {
	if h.browserDisabled(w) {
		return
	}
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	p := h.principal(r)
	for _, s := range h.browserOf().List() {
		if s.SessionID == sess.ID && s.TenantID == p.TenantID {
			writeJSON(w, http.StatusOK, s)
			return
		}
	}
	fail(w, http.StatusNotFound, "该会话没有运行中的浏览器")
}

func (h *handler) closeBrowser(w http.ResponseWriter, r *http.Request) {
	if h.browserDisabled(w) {
		return
	}
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}
	p := h.principal(r)
	for _, s := range h.browserOf().List() {
		if s.SessionID == sess.ID && s.TenantID == p.TenantID {
			_ = h.browserOf().Close(s.ID)
			h.audit(r, "browser.close", "session", sess.ID, map[string]any{"browser": s.ID})
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) browserFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browser.ErrBusy):
		fail(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, browser.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, browser.ErrDisabled):
		fail(w, http.StatusServiceUnavailable, err.Error())
	default:
		h.failErr(w, err)
	}
}
