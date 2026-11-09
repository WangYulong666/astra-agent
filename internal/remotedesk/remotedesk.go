// Package remotedesk 是「手机看/控电脑」能力。
//
// 分层（见 docs/design/remote-desktop.md）：监看 / 快捷操作 / 键鼠接管 / 指令通道。
// 每层单独可用：缺屏幕录制权限就只剩快捷操作，缺辅助功能权限就只读不控。
//
// 本文件是并行任务的**冻结契约**，任何实现改动只放在同包其它文件里。
package remotedesk

import (
	"context"
	"errors"
	"log"
	"time"
)

// 会话模式。
const (
	ModeView    = "view"    // 只看画面
	ModeControl = "control" // 可注入鼠标键盘
)

// 会话状态。
const (
	StatusPending = "pending_approval" // 等桌面端批准（control 默认）
	StatusActive  = "active"
	StatusEnded   = "ended"
	StatusDenied  = "denied"
	StatusExpired = "expired"
)

// 状态原因，写进 Session.EndReason / 审计。
const (
	ReasonUserEnded   = "user_ended"
	ReasonRevoked     = "device_revoked"
	ReasonTTL         = "ttl_expired"
	ReasonShutdown    = "server_shutdown"
	ReasonDenied      = "denied_on_desktop"
	ReasonSuperseded  = "superseded"
	ReasonDisabled    = "remote_disabled"
	ReasonInputClose  = "input_unavailable"
	ReasonStreamError = "stream_error"
)

// 快捷操作动作名（L2：不需要辅助功能权限）。
const (
	ActionOpenApp    = "open_app"   // Args["name"] 应用名，如 "Finder"
	ActionOpenURL    = "open_url"   // Args["url"]，仅 http/https
	ActionScreenshot = "screenshot" // 存到 <dataDir>/remote/shots，返回路径
	ActionLock       = "lock"       // 锁屏 / 立即熄屏
)

// ErrNotFound 会话/设备/配对码不存在；跨租户也回这个（不区分「不存在」与「无权」，避免探测）。
var ErrNotFound = errors.New("remotedesk: 不存在")

// ErrDenied 明确被拒（配对码错太多次、设备已吊销、输入未授权）。
var ErrDenied = errors.New("remotedesk: 被拒绝")

// ErrUnavailable 该层能力在本机不可用（缺权限/缺工具）。
var ErrUnavailable = errors.New("remotedesk: 能力不可用")

// ── 能力探测 ────────────────────────────────────────────────────

// Capability 描述某一层能力在本机的可用性。不可用时 Reason 必须写清原因，
// Hint 给可执行的修复动作（例如「系统设置 → 隐私与安全 → 辅助功能」）。
type Capability struct {
	Available bool   `json:"available"`
	Backend   string `json:"backend,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type Screen struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Capabilities 是 GET /v1/remote/capabilities 的响应。
type Capabilities struct {
	Platform string     `json:"platform"`
	Capture  Capability `json:"capture"`
	Input    Capability `json:"input"`
	Actions  Capability `json:"actions"`
	Screen   Screen     `json:"screen"`
	MaxFPS   int        `json:"maxFps"`
	// Notice 是给人看的一句话总结，例如「本机未授予辅助功能权限，只能看不能控」。
	Notice string `json:"notice,omitempty"`
}

// ── 配对与设备 ──────────────────────────────────────────────────

// Pairing 是桌面端生成的一次性配对码。
type Pairing struct {
	Code      string    `json:"code"`      // 6 位数字
	ExpiresAt time.Time `json:"expiresAt"` // 默认 120 秒
	// Payload 是二维码内容，手机扫它即可带出 code（前端画 QR 不引依赖，见 R4）。
	Payload string `json:"payload"`
	// DeepLink 供原生 App 直接跳转，如 astra://remote/pair?code=123456&server=http://...
	DeepLink string `json:"deepLink,omitempty"`
}

type PairingRequest struct {
	TenantID string
	UserID   string
	Device   string // 设备名（默认取 hostname）
}

type ClaimRequest struct {
	Code     string
	TenantID string
	UserID   string
	Device   string
	Platform string
}

type Device struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"-"`
	UserID     string     `json:"-"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeenAt time.Time  `json:"lastSeenAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	// Online 由服务端根据当前是否有活动会话推导，不落盘。
	Online bool `json:"online"`
}

// ── 会话 ────────────────────────────────────────────────────────

type StartRequest struct {
	TenantID string
	UserID   string
	DeviceID string
	Mode     string // ModeView / ModeControl
	// RequireApproval 为空时用 Policy.ControlNeedsApproval（control 默认 true，view 默认 false）。
	RequireApproval *bool
	Client          string // 来源描述，如 "iOS / Safari"，进审计
}

type Session struct {
	ID         string     `json:"id"`
	DeviceID   string     `json:"deviceId"`
	Device     string     `json:"device,omitempty"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	EndReason  string     `json:"endReason,omitempty"`
	Client     string     `json:"client,omitempty"`
	// Stats 由 Runtime 实时更新，列表接口带上是方便运营看「这路有没有在动」。
	Stats    Stats  `json:"stats"`
	TenantID string `json:"-"`
	UserID   string `json:"-"`
}

// ── 画面与输入 ──────────────────────────────────────────────────

// Frame 是一帧画面（JPEG）。Width/Height 是编码后的尺寸，便于前端换算坐标。
type Frame struct {
	JPEG   []byte
	Width  int
	Height int
	At     time.Time
}

// InputEvent 是手机端发来的一次输入。坐标为**归一化 0..1**（相对编码后画面），
// 注入层按实际屏幕尺寸换算——手机分辨率/画质档位变化时不用改客户端逻辑。
type InputEvent struct {
	Op   string  `json:"op"` // move / down / up / click / dblclick / rightclick / wheel / key / text
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	DX   int     `json:"dx,omitempty"`
	DY   int     `json:"dy,omitempty"`
	Key  string  `json:"key,omitempty"`  // 键名：Enter / Backspace / Tab / Escape / ArrowUp / a ...
	Text string  `json:"text,omitempty"` // 整段文本（虚拟键盘）
	Mods string  `json:"mods,omitempty"` // "cmd,shift"
}

// Quality 是客户端画质档位。零值表示用服务端默认。
type Quality struct {
	MaxWidth int `json:"maxWidth,omitempty"` // 0 用 Policy.MaxWidth
	FPS      int `json:"fps,omitempty"`      // 0 用 Policy.MaxFPS
	Quality  int `json:"quality,omitempty"`  // JPEG 质量 1..100，0 用 Policy.Quality
}

// Stats 是这一路流的水位，前端用来显示「掉帧/帧率」。
type Stats struct {
	FPS     float64 `json:"fps"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Quality int     `json:"quality"`
	Frames  uint64  `json:"frames"`
	Dropped uint64  `json:"dropped"`
	Inputs  uint64  `json:"inputs"`
}

// Runtime 是一个已打开的会话通道（一个手机客户端对应一个）。
// 多个客户端可同时 Open 同一会话，抓屏泵共享、各自订阅。
type Runtime interface {
	// Frames 订阅画面。通道满时丢最旧帧（不阻塞抓屏）；ctx 结束时通道关闭。
	Frames(ctx context.Context) (<-chan Frame, error)
	// Input 注入一次输入。未授权/不可用时返回 ErrUnavailable；超速返回 ErrDenied。
	Input(ev InputEvent) error
	// SetQuality 调整画质档位，立即对后续帧生效。
	SetQuality(q Quality) error
	Stats() Stats
	// Close 退订。不结束会话本身（会话由 EndSession 结束）。
	Close() error
}

// ── 快捷操作 ────────────────────────────────────────────────────

type ActionRequest struct {
	SessionID string
	Action    string
	Args      map[string]string
	// 归属，由 API 层从 principal 填入并校验。
	TenantID string
	UserID   string
}

type ActionResult struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Path 仅 screenshot 用。
	Path string `json:"path,omitempty"`
}

// ── 审计 ────────────────────────────────────────────────────────

// AuditSink 记录远程桌面动作。nil 时静默丢弃（与平台其它可选能力一致）。
// 不记录输入内容（键盘输入可能含密码），只记动作与计数。
type AuditSink func(action, target string, detail map[string]any)

// ── 装配 ────────────────────────────────────────────────────────

// Policy 是默认策略。零值字段由 Open 填默认值。
type Policy struct {
	PairingTTL           time.Duration // 120s
	PairingMaxAttempts   int           // 5
	SessionTTL           time.Duration // 4h
	ControlNeedsApproval bool          // true
	MaxFPS               int           // 10
	MaxWidth             int           // 1600
	Quality              int           // 60
	MaxInputRate         int           // 200/s
	// DisableInput 用否定式：零值 = 允许（bool 的「默认 true」没法用零值表达，
	// 写成 AllowInput 会让忘记赋值的人意外关掉输入）。
	DisableInput bool // true 时 control 会话直接不可建
}

type Config struct {
	DataDir string // 设备/会话/截图落盘根目录（<dataDir>/remote）
	// Tenancy 为 true 时强制租户归属（多租户）。false 是单机兼容模式。
	Tenancy bool
	Policy  Policy
	Logger  *log.Logger
	Audit   AuditSink
	// Disabled 是全局急停（ASTRA_REMOTE_DISABLED）。
	Disabled bool

	// Capture / Input 允许注入假后端（测试、无权限环境演示）。
	// nil 时用平台默认探测。
	Capture CaptureFactory
	Input   InputFactory
}

// GrabOptions 是单次抓屏参数。
type GrabOptions struct {
	MaxWidth int // 0 = 原始
	Quality  int // 1..100
}

// Capture 是抓屏后端。实现见 capture*.go（R2）。
type Capture interface {
	Info() Capability
	Grab(ctx context.Context, opt GrabOptions) (Frame, error)
	Close() error
}

// Input 是键鼠注入后端。实现见 input*.go（R2）。
type Input interface {
	Info() Capability
	// Apply 注入一次输入；screen 是实际屏幕尺寸，用于把归一化坐标换算成像素。
	Apply(ctx context.Context, ev InputEvent, screen Screen) error
	Close() error
}

type CaptureFactory func(cfg Config) (Capture, error)
type InputFactory func(cfg Config) (Input, error)

// Manager 是远程桌面的全部能力入口。
type Manager interface {
	// Capabilities 探测本机三层的可用性（结果缓存 30 秒）。
	Capabilities(ctx context.Context) (Capabilities, error)

	StartPairing(ctx context.Context, req PairingRequest) (Pairing, error)
	ClaimPairing(ctx context.Context, req ClaimRequest) (Device, error)
	ListDevices(ctx context.Context, tenantID, userID string) ([]Device, error)
	RevokeDevice(ctx context.Context, tenantID, userID, deviceID string) error

	StartSession(ctx context.Context, req StartRequest) (Session, error)
	GetSession(ctx context.Context, tenantID, sessionID string) (Session, error)
	ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error)
	ApproveSession(ctx context.Context, tenantID, sessionID string) (Session, error)
	EndSession(ctx context.Context, tenantID, sessionID, reason string) error

	Open(ctx context.Context, tenantID, sessionID string) (Runtime, error)
	RunAction(ctx context.Context, req ActionRequest) (ActionResult, error)

	// Close 结束所有会话、停抓屏泵、刷盘。可重复调用。
	Close() error
}

// Open 装配管理器。实现见 manager.go（R1）。
func Open(cfg Config) (Manager, error) { return openImpl(cfg) }

// FillPolicy 补默认值。R1/R2 共用，避免两处默认值漂移。
func (p Policy) WithDefaults() Policy {
	if p.PairingTTL <= 0 {
		p.PairingTTL = 120 * time.Second
	}
	if p.PairingMaxAttempts <= 0 {
		p.PairingMaxAttempts = 5
	}
	if p.SessionTTL <= 0 {
		p.SessionTTL = 4 * time.Hour
	}
	if p.MaxFPS <= 0 {
		p.MaxFPS = 10
	}
	if p.MaxFPS > 30 {
		p.MaxFPS = 30
	}
	if p.MaxWidth <= 0 {
		p.MaxWidth = 1600
	}
	if p.Quality <= 0 {
		p.Quality = 60
	}
	if p.Quality > 95 {
		p.Quality = 95
	}
	if p.MaxInputRate <= 0 {
		p.MaxInputRate = 200
	}
	return p
}
