// Package browser 给 agent 一个**真正的浏览器**。
//
// 为什么需要：Grok Bot 的说法是能处理「no clean API or MCP」的平台。缺了浏览器，
// agent 就只能操作有 API 的东西，而现实里大量系统只有网页。
//
// 形态选择：不把 Playwright 塞进沙箱镜像（镜像会重到没法维护，而且每个
// provider 的兼容性都不一样），而是**平台侧起一个 Chrome，把 CDP 地址给 agent**。
// agent 在沙箱里用 playwright/chromedp 连过去即可，沙箱本身不需要装浏览器。
//
// 注意与 egress 的关系：浏览器的流量走**代理进程自己的网络栈**，不经过沙箱的
// 出网代理。所以浏览器必须单独应用同一套策略——在 Launch 时把策略快照带进去，
// 由 browser 包的下载/导航钩子校验（见 LaunchRequest.Policy）。
package browser

import (
	"context"
	"errors"
	"time"
)

// Config 装配。
type Config struct {
	// Enabled 关掉时 Open 返回 Disabled。
	Enabled bool
	// ChromePath 可执行文件路径；空则按平台默认位置探测
	// （macOS: /Applications/Google Chrome.app/Contents/MacOS/Google Chrome，
	//  Linux: /usr/bin/google-chrome / chromium）。
	ChromePath string
	// MaxSessions 同时存活的上限，默认 4。超了返回 ErrBusy 而不是把机器耗死。
	MaxSessions int
	// IdleTimeout 空闲多久自动关，默认 30 分钟。
	IdleTimeout time.Duration
	// Headless 默认 true；调试时可以带 false 起有界面的。
	Headless bool
}

// Session 是一个活的浏览器实例。
type Session struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"-"`
	SessionID string `json:"session_id,omitempty"` // 平台会话 id
	RunID     string `json:"run_id,omitempty"`
	// CDPURL 是 agent 要连的地址，形如 ws://127.0.0.1:PORT/devtools/browser/UUID。
	CDPURL string `json:"cdp_url"`
	// HTTPURL 给只需要 HTTP 端点的工具（/json/version）。
	HTTPURL   string    `json:"http_url"`
	StartedAt time.Time `json:"started_at"`
	LastUsed  time.Time `json:"last_used"`
	Pages     int       `json:"pages"`
}

// LaunchRequest 起一个浏览器。
type LaunchRequest struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
	// AllowedHosts 是导航白名单。空表示不限制（仅本地调试）。
	// 拦截发生在 browser 侧的导航钩子，不依赖沙箱。
	AllowedHosts []string
	// Profile 复用哪个用户数据目录。空表示每次新建（用完即焚，最安全）。
	// 传 "persist" 表示按 user 维度持久化 cookie——便利与隔离的取舍点。
	Profile string
}

// Manager 是浏览器能力的对外面。
type Manager interface {
	Enabled() bool
	Launch(ctx context.Context, req LaunchRequest) (Session, error)
	Get(id string) (Session, bool)
	List() []Session
	Close(id string) error
	CloseAll()
}

var (
	ErrDisabled = errors.New("browser: 未启用（未找到 Chrome）")
	ErrBusy     = errors.New("browser: 并发浏览器实例已达上限")
	ErrNotFound = errors.New("browser: 实例不存在或已关闭")
)
