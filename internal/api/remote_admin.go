package api

// 远程桌面的管理面（HTTP）：能力探测、配对、设备、会话、快捷操作。
//
// 三条硬规矩，改这个文件前先读一遍：
//  1. 归属只认 principal 的 TenantID/UserID。请求体/query 里的 tenant 一律不看——
//     允许它等于允许别人往你租户名下开一路「看你屏幕」的通道。
//  2. 审计 detail 不放配对码、键盘输入、URL 的 query。审计表很多人看，
//     这三样拼起来就是一次可复现的越权（配对码 = 绑定别人的手机，
//     键盘输入 = 密码，URL query = 各种 token）。
//  3. 错误映射集中在 failRemote：NotFound/Denied 不解释原因（不区分「不存在」与
//     「无权」，避免探测）；Unavailable 一定把原因带出去——用户得知道缺的是
//     屏幕录制权限还是辅助功能权限，否则只会看到一句「不可用」。
//
// 画面传输（WebSocket）在 remote_ws.go，不在这里。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/remotedesk"
)

// remoteBodyLimit 请求体上限。这些接口的 body 最大也就是一个 URL + 几个字段，
// 64KB 已经宽松到不合理，再大就是有人在喂垃圾。
const remoteBodyLimit = 64 << 10

// remoteReady 统一处理「能力未启用」。每个 handler 第一句都调它：
// 少了这句，nil manager 会在 handler 深处空指针 panic，被 withRecover 接住后
// 变成 500，用户看到的是「服务器内部错误」而不是「没配」。
func (h *handler) remoteReady(w http.ResponseWriter) bool {
	if h.cfg.Remote == nil {
		fail(w, http.StatusServiceUnavailable,
			"远程桌面未启用（ASTRA_REMOTE_DISABLED 或探测失败，看 GET /v1/remote/capabilities）")
		return false
	}
	return true
}

// failRemote 把 remotedesk 的三类语义错误映射成 HTTP 状态码。
//
// 注意 ErrUnavailable 必须带原因：它专门用于「本机缺权限/缺工具」，
// 而这类问题用户自己能修（去系统设置里勾一下），带着原因才有意义。
func (h *handler) failRemote(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, remotedesk.ErrNotFound):
		// 跨租户也走这条：403 等于告诉对方「这个 ID 存在」。
		fail(w, http.StatusNotFound, "设备或会话不存在")
	case errors.Is(err, remotedesk.ErrDenied):
		fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, remotedesk.ErrUnavailable):
		fail(w, http.StatusServiceUnavailable, err.Error())
	default:
		h.failErr(w, err)
	}
}

// decodeRemoteBody 限长解析 JSON 请求体；返回 false 表示已经写过响应。
//
// 允许空 body：前端首屏可能直接 POST 一个空对象，甚至什么都不发。
func decodeRemoteBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, remoteBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "请求体过大（上限 64KB）")
			return false
		}
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// ── 能力 ────────────────────────────────────────────────────────

func (h *handler) remoteCapabilities(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	caps, err := h.cfg.Remote.Capabilities(r.Context())
	if err != nil {
		h.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, caps)
}

// ── 配对 ────────────────────────────────────────────────────────

func (h *handler) remoteStartPairing(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	var body struct {
		Device string `json:"device"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	p := h.principal(r)
	pair, err := h.cfg.Remote.StartPairing(r.Context(), remotedesk.PairingRequest{
		TenantID: p.TenantID,
		UserID:   p.UserID,
		Device:   body.Device,
	})
	if err != nil {
		h.failRemote(w, err)
		return
	}
	// 配对码绝不进审计：它是「人在电脑跟前」的证明，写进日志等于把证明公开。
	h.audit(r, "remote.pair", "device", "", map[string]any{
		"device":    body.Device,
		"expiresAt": pair.ExpiresAt,
	})
	writeJSON(w, http.StatusCreated, pair)
}

func (h *handler) remoteClaimPairing(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	var body struct {
		Code     string `json:"code"`
		Device   string `json:"device"`
		Platform string `json:"platform"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		fail(w, http.StatusBadRequest, "code 不能为空")
		return
	}
	p := h.principal(r)
	dev, err := h.cfg.Remote.ClaimPairing(r.Context(), remotedesk.ClaimRequest{
		Code:     strings.TrimSpace(body.Code),
		TenantID: p.TenantID,
		UserID:   p.UserID,
		Device:   body.Device,
		Platform: body.Platform,
	})
	if err != nil {
		h.failRemote(w, err)
		return
	}
	// 同样不记 code：记了它，这条审计就成了「重放一次配对」的说明书。
	h.audit(r, "remote.claim", "device", dev.ID, map[string]any{
		"name":     dev.Name,
		"platform": dev.Platform,
	})
	writeJSON(w, http.StatusCreated, dev)
}

// ── 设备 ────────────────────────────────────────────────────────

func (h *handler) remoteListDevices(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	p := h.principal(r)
	devices, err := h.cfg.Remote.ListDevices(r.Context(), p.TenantID, p.UserID)
	if err != nil {
		h.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(devices)})
}

func (h *handler) remoteRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		fail(w, http.StatusBadRequest, "缺少设备 id")
		return
	}
	if err := h.cfg.Remote.RevokeDevice(r.Context(), p.TenantID, p.UserID, id); err != nil {
		h.failRemote(w, err)
		return
	}
	h.audit(r, "remote.revoke", "device", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ── 会话 ────────────────────────────────────────────────────────

func (h *handler) remoteStartSession(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Mode     string `json:"mode"`
		Client   string `json:"client"`
		// 指针：三态。nil 表示「按服务端策略」，false 表示「我明确不要审批」。
		RequireApproval *bool `json:"requireApproval"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	// 模式在这里就挡掉：交给 manager 的话它会回 ErrDenied（403），
	// 而「传了个不存在的模式」是客户端写错了，400 才是对的。
	mode := strings.TrimSpace(body.Mode)
	switch mode {
	case remotedesk.ModeView, remotedesk.ModeControl:
	default:
		fail(w, http.StatusBadRequest, "mode 只能是 view 或 control")
		return
	}
	deviceID := strings.TrimSpace(body.DeviceID)
	if deviceID == "" {
		fail(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	p := h.principal(r)
	sess, err := h.cfg.Remote.StartSession(r.Context(), remotedesk.StartRequest{
		TenantID:        p.TenantID,
		UserID:          p.UserID,
		DeviceID:        deviceID,
		Mode:            mode,
		RequireApproval: body.RequireApproval,
		Client:          body.Client,
	})
	if err != nil {
		h.failRemote(w, err)
		return
	}
	h.audit(r, "remote.session.start", "session", sess.ID, map[string]any{
		"device": sess.DeviceID,
		"mode":   sess.Mode,
		"status": sess.Status,
		"client": sess.Client,
	})
	writeJSON(w, http.StatusCreated, sess)
}

func (h *handler) remoteListSessions(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	// 解析不出来就当没给（manager 会补默认值），不为一个可选的展示参数报 400。
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	p := h.principal(r)
	sessions, err := h.cfg.Remote.ListSessions(r.Context(), p.TenantID, limit)
	if err != nil {
		h.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(sessions)})
}

func (h *handler) remoteGetSession(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	p := h.principal(r)
	sess, err := h.cfg.Remote.GetSession(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		h.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *handler) remoteApproveSession(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	sess, err := h.cfg.Remote.ApproveSession(r.Context(), p.TenantID, id)
	if err != nil {
		h.failRemote(w, err)
		return
	}
	h.audit(r, "remote.session.approve", "session", sess.ID, map[string]any{
		"device": sess.DeviceID,
		"mode":   sess.Mode,
		"status": sess.Status,
	})
	writeJSON(w, http.StatusOK, sess)
}

func (h *handler) remoteEndSession(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	if err := h.cfg.Remote.EndSession(r.Context(), p.TenantID, id, remotedesk.ReasonUserEnded); err != nil {
		h.failRemote(w, err)
		return
	}
	h.audit(r, "remote.session.end", "session", id, map[string]any{
		"reason": remotedesk.ReasonUserEnded,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── 快捷操作（L2）──────────────────────────────────────────────

func (h *handler) remoteAction(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	var body struct {
		Action string            `json:"action"`
		Args   map[string]string `json:"args"`
	}
	if !decodeRemoteBody(w, r, &body) {
		return
	}
	action := strings.TrimSpace(body.Action)
	if action == "" {
		fail(w, http.StatusBadRequest, "action 不能为空")
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	res, err := h.cfg.Remote.RunAction(r.Context(), remotedesk.ActionRequest{
		SessionID: id,
		Action:    action,
		Args:      body.Args,
		TenantID:  p.TenantID,
		UserID:    p.UserID,
	})
	if err != nil {
		h.failRemote(w, err)
		return
	}

	detail := map[string]any{"action": action, "ok": res.OK}
	// open_url 只记 host：path 和 query 里经常带一次性 token，
	// 而我们需要的只是「他打开了哪个站」这个量级的信息。
	if action == remotedesk.ActionOpenURL {
		if u, perr := url.Parse(strings.TrimSpace(body.Args["url"])); perr == nil && u.Host != "" {
			detail["host"] = u.Host
		}
	}
	h.audit(r, "remote.action", "session", id, detail)
	writeJSON(w, http.StatusOK, res)
}
