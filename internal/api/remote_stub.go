package api

// ⚠ 临时桩：由 R3 删除本文件，换成 remote_admin.go + remote_ws.go 的真实现。
// 主控先占位，保证骨架期编译通过、路由已注册、契约测试可跑。

import "net/http"

func (h *handler) remoteNotReady(w http.ResponseWriter) bool {
	if h.cfg.Remote == nil {
		fail(w, http.StatusServiceUnavailable, "远程桌面未启用（ASTRA_REMOTE_DISABLED 或探测失败，看 GET /v1/remote/capabilities）")
		return true
	}
	return false
}

func (h *handler) remoteCapabilities(w http.ResponseWriter, r *http.Request) {
	h.remoteNotReady(w)
}

func (h *handler) remoteStartPairing(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteClaimPairing(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteListDevices(w http.ResponseWriter, r *http.Request)  { h.remoteNotReady(w) }
func (h *handler) remoteRevokeDevice(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteStartSession(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteListSessions(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteGetSession(w http.ResponseWriter, r *http.Request)   { h.remoteNotReady(w) }
func (h *handler) remoteApproveSession(w http.ResponseWriter, r *http.Request) {
	h.remoteNotReady(w)
}
func (h *handler) remoteEndSession(w http.ResponseWriter, r *http.Request) { h.remoteNotReady(w) }
func (h *handler) remoteStream(w http.ResponseWriter, r *http.Request)     { h.remoteNotReady(w) }
func (h *handler) remoteAction(w http.ResponseWriter, r *http.Request)     { h.remoteNotReady(w) }
