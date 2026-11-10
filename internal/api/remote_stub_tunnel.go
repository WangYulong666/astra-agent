package api

import "net/http"

// ⚠ 临时桩：W1 实现 relay 后由主控替换成读 h.cfg.Tunnel() 的真实现。
func (h *handler) remoteTunnel(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tunnel == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	st := h.cfg.Tunnel()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "connected": st.Connected, "url": st.URL,
		"deviceId": st.DeviceID, "streams": st.Streams,
		"reconnects": st.Reconnects, "lastError": st.LastError,
	})
}
