package client

// 远程桌面（手机看/控电脑）的协议翻译层。
//
// 为什么单独一个文件：这十条接口是一个完整的产品面（能力探测 → 配对 → 设备 →
// 会话 → 快捷操作），塞进 admin.go 会让那个文件继续膨胀，而它们之间共享的
// 只有一串路径前缀。
//
// 两条约定（与 internal/remotedesk 的契约对齐，但不 import 它）：
//   - client 是给外部程序用的，只认 JSON 形状，不拖进服务端内部包；
//   - 字段名一律跟服务端一致（camelCase），不做本地重命名——脚本按 API
//     契约写，不按 CLI 的措辞写。
//
// 归属由服务端从 principal 推导，这里**没有任何 tenant/user 参数**：
// 允许客户端指定租户，等于允许别人往你名下开一路「看你屏幕」的通道。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ── 能力探测 ────────────────────────────────────────────────────

// RemoteCapability 是某一层能力在本机的可用性。不可用时 Reason 说原因，
// Hint 给可执行的修复动作（例如「系统设置 → 隐私与安全 → 辅助功能」）。
type RemoteCapability struct {
	Available bool   `json:"available"`
	Backend   string `json:"backend,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type RemoteScreen struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// RemoteCapabilities 是 GET /v1/remote/capabilities 的响应。
//
// 三层分开报，是因为缺权限时**不该整块消失**：没有辅助功能权限时
// 监看与快捷操作仍然是可用的，这正是「只读模式」的来源。
type RemoteCapabilities struct {
	Platform string           `json:"platform"`
	Capture  RemoteCapability `json:"capture"`
	Input    RemoteCapability `json:"input"`
	Actions  RemoteCapability `json:"actions"`
	Screen   RemoteScreen     `json:"screen"`
	MaxFPS   int              `json:"maxFps"`
	Notice   string           `json:"notice,omitempty"`
}

// RemoteCapabilities 探测本机三层能力。结果由服务端缓存 30 秒，可以放心轮询。
func (c *Client) RemoteCapabilities(ctx context.Context) (RemoteCapabilities, error) {
	var out RemoteCapabilities
	err := c.do(ctx, http.MethodGet, "/v1/remote/capabilities", nil, nil, &out)
	return out, err
}

// ── 配对与设备 ──────────────────────────────────────────────────

// RemotePairing 是一次性配对码。Code 是 6 位数字，TTL 120 秒且只能用一次。
type RemotePairing struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Payload 是二维码内容，手机扫它即可带出 code。
	Payload string `json:"payload"`
	// DeepLink 供原生 App 直接跳转，可能为空（老服务端没有这个字段）。
	DeepLink string `json:"deepLink,omitempty"`
}

// Remaining 返回配对码剩余有效期，已过期返回 0。
// 用它而不是直接减 ExpiresAt：时钟偏差或服务端已过期时，负数对用户毫无意义。
func (p RemotePairing) Remaining(now time.Time) time.Duration {
	d := p.ExpiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// RemotePair 在桌面端生成配对码。device 为空时服务端用 hostname。
// 配对码只证明「人在电脑跟前」，不代表任何额外权限——真正的归属始终是登录态。
func (c *Client) RemotePair(ctx context.Context, device string) (RemotePairing, error) {
	body := struct {
		Device string `json:"device,omitempty"`
	}{Device: device}
	var out RemotePairing
	err := c.do(ctx, http.MethodPost, "/v1/remote/pairing", nil, body, &out)
	return out, err
}

// RemoteDevice 是一台已绑定的设备。RevokedAt 非空即已吊销（会话同时被断开）。
type RemoteDevice struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeenAt time.Time  `json:"lastSeenAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	// Online 由服务端按「当前是否有活动会话」推导，不落盘。
	Online bool `json:"online"`
}

// RemoteClaim 用配对码把本机绑定成设备（手机侧 / 脚本用）。
// code 为空是调用方写错了，本地就挡掉，不发一个必然 400 的请求。
func (c *Client) RemoteClaim(ctx context.Context, code, device string) (RemoteDevice, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return RemoteDevice{}, &UseError{Msg: "缺少配对码"}
	}
	body := struct {
		Code   string `json:"code"`
		Device string `json:"device,omitempty"`
	}{Code: code, Device: device}
	var out RemoteDevice
	err := c.do(ctx, http.MethodPost, "/v1/remote/pairing/claim", nil, body, &out)
	return out, err
}

// RemoteDevices 列出当前身份自己的设备（服务端按 principal 过滤，没有跨租户参数）。
func (c *Client) RemoteDevices(ctx context.Context) ([]RemoteDevice, error) {
	var out struct {
		Items []RemoteDevice `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/remote/devices", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []RemoteDevice{}
	}
	return out.Items, nil
}

// RemoteRevokeDevice 吊销设备。服务端会级联断开该设备上的活动会话，立即生效。
func (c *Client) RemoteRevokeDevice(ctx context.Context, id string) error {
	if id == "" {
		return &UseError{Msg: "缺少设备 id"}
	}
	path := fmt.Sprintf("/v1/remote/devices/%s", url.PathEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ── 会话 ────────────────────────────────────────────────────────

// 会话模式。字符串与 remotedesk.ModeView/ModeControl 一致（协议值，不是常量引用）。
const (
	RemoteModeView    = "view"
	RemoteModeControl = "control"
)

// 会话状态。control 会话默认从 pending_approval 开始，要桌面端批准才转 active。
const (
	RemoteStatusPending = "pending_approval"
	RemoteStatusActive  = "active"
	RemoteStatusEnded   = "ended"
	RemoteStatusDenied  = "denied"
	RemoteStatusExpired = "expired"
)

// RemoteStats 是这一路流的水位。列表接口带上是方便运营看「这路有没有在动」。
type RemoteStats struct {
	FPS     float64 `json:"fps"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Quality int     `json:"quality"`
	Frames  uint64  `json:"frames"`
	Dropped uint64  `json:"dropped"`
	Inputs  uint64  `json:"inputs"`
}

type RemoteSession struct {
	ID         string      `json:"id"`
	DeviceID   string      `json:"deviceId"`
	Device     string      `json:"device,omitempty"`
	Mode       string      `json:"mode"`
	Status     string      `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	ApprovedAt *time.Time  `json:"approvedAt,omitempty"`
	EndedAt    *time.Time  `json:"endedAt,omitempty"`
	EndReason  string      `json:"endReason,omitempty"`
	Client     string      `json:"client,omitempty"`
	Stats      RemoteStats `json:"stats"`
}

// RemoteStartSession 建一路会话。mode 只能是 view 或 control（服务端 400 会明确说）。
func (c *Client) RemoteStartSession(ctx context.Context, deviceID, mode string) (RemoteSession, error) {
	if deviceID == "" {
		return RemoteSession{}, &UseError{Msg: "缺少设备 id（先 remote devices 看有哪些）"}
	}
	body := struct {
		DeviceID string `json:"deviceId"`
		Mode     string `json:"mode"`
	}{DeviceID: deviceID, Mode: mode}
	var out RemoteSession
	err := c.do(ctx, http.MethodPost, "/v1/remote/sessions", nil, body, &out)
	return out, err
}

// RemoteSessions 列会话（按创建时间倒序）。limit<=0 用 20。
func (c *Client) RemoteSessions(ctx context.Context, limit int) ([]RemoteSession, error) {
	if limit <= 0 {
		limit = 20
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	var out struct {
		Items []RemoteSession `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/remote/sessions", params, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []RemoteSession{}
	}
	return out.Items, nil
}

// RemoteSession 取单个会话（含实时 Stats）。
func (c *Client) RemoteSession(ctx context.Context, id string) (RemoteSession, error) {
	if id == "" {
		return RemoteSession{}, &UseError{Msg: "缺少会话 id"}
	}
	var out RemoteSession
	path := fmt.Sprintf("/v1/remote/sessions/%s", url.PathEscape(id))
	err := c.do(ctx, http.MethodGet, path, nil, nil, &out)
	return out, err
}

// RemoteApprove 批准接管（pending_approval → active）。这是「人在电脑跟前」的
// 第二次确认：配对只证明绑定那一刻，接管要再点一次。
func (c *Client) RemoteApprove(ctx context.Context, id string) (RemoteSession, error) {
	if id == "" {
		return RemoteSession{}, &UseError{Msg: "缺少会话 id"}
	}
	var out RemoteSession
	path := fmt.Sprintf("/v1/remote/sessions/%s/approve", url.PathEscape(id))
	err := c.do(ctx, http.MethodPost, path, nil, nil, &out)
	return out, err
}

// RemoteEndSession 急停：任意一方都可断，服务端立即关闭抓屏泵与所有订阅者。
func (c *Client) RemoteEndSession(ctx context.Context, id string) error {
	if id == "" {
		return &UseError{Msg: "缺少会话 id"}
	}
	path := fmt.Sprintf("/v1/remote/sessions/%s", url.PathEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ── 快捷操作（L2：不需要辅助功能权限）────────────────────────────

// 快捷操作名。与 remotedesk.Action* 的协议值一致。
const (
	RemoteActionOpenApp    = "open_app"   // Args["name"]
	RemoteActionOpenURL    = "open_url"   // Args["url"]，仅 http/https
	RemoteActionScreenshot = "screenshot" // 存到 <dataDir>/remote/shots
	RemoteActionLock       = "lock"       // 锁屏 / 立即熄屏
)

type RemoteActionResult struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Path 仅 screenshot 用，是服务端落盘的绝对路径。
	Path string `json:"path,omitempty"`
}

// RemoteAction 在一次会话上执行快捷操作。args 为 nil 时不带 args 字段。
//
// 为什么挂在 session 上而不是 device 上：快捷操作要进审计，审计要能顺着
// 「谁在什么时候看/控了这台机器」查回去，而会话就是那条记录。
func (c *Client) RemoteAction(ctx context.Context, sessionID, action string, args map[string]string) (RemoteActionResult, error) {
	if sessionID == "" {
		return RemoteActionResult{}, &UseError{Msg: "缺少会话 id"}
	}
	if action == "" {
		return RemoteActionResult{}, &UseError{Msg: "缺少动作名"}
	}
	body := struct {
		Action string            `json:"action"`
		Args   map[string]string `json:"args,omitempty"`
	}{Action: action, Args: args}
	var out RemoteActionResult
	path := fmt.Sprintf("/v1/remote/sessions/%s/actions", url.PathEscape(sessionID))
	err := c.do(ctx, http.MethodPost, path, nil, body, &out)
	return out, err
}
