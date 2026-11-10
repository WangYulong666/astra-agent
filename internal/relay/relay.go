// Package relay 是「让手机在外网连上家里电脑」的反向隧道。
//
// 为什么需要它：WorkBuddy 的远程控制之所以「1 分钟配好」，不是画面技术好，
// 而是电脑端**主动出站**建长连接、云端中继记住这条隧道，用户不用碰路由器。
// 我们 v1 把这一步推给了「自备反向代理 + TLS + 域名」，等于把最难的一步
// 留给用户——这是 v2 必须补的洞。
//
// 形态：设备端（用户电脑）主动连 relay；手机连 relay 的 /t/{device}/...
// relay 用 smux 在一条 WSS 上多路复用，每个 HTTP 请求一条逻辑流，
// 设备端把流原样接到本地控制面。relay **不做 TLS 终结以外的任何解析**，
// 鉴权仍由控制面（origin）执行——relay 只是一根管道。
package relay

import (
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"time"
)

var (
	ErrUnknownDevice = errors.New("relay: 设备未连接")
	ErrUnauthorized  = errors.New("relay: 令牌无效")
)

// ServerConfig 是公网中继的配置。
type ServerConfig struct {
	Addr string // 例："127.0.0.1:9443"；":0" 由内核分配（测试用）
	// Token 是设备注册令牌（共享密钥，HMAC 校验）。
	// 为空表示「本机调试模式」：只允许 127.0.0.1 连接，启动时打告警。
	Token string
	// PublicURL 用于日志与 SelfCheck，例 "https://relay.example.com"。
	PublicURL string
	Logger    *log.Logger
	// TLS 为 nil 时监听明文（仅本机验证）；线上必须给证书。
	TLS *tls.Config
	// MaxStreams 限制单设备并发流；0 用 128。
	MaxStreams int
	// IdleTimeout 是隧道无数据的断开阈值；0 用 90 秒。
	IdleTimeout time.Duration
}

// Server 是 relay 服务端。
type Server interface {
	// Addr 返回实际监听地址（Addr 为 :0 时由内核决定）。
	Addr() string
	// Handler 是给 http.Server 用的处理器：
	//   GET /_relay/healthz        健康检查（无需鉴权）
	//   GET /t/{device}/<path>     反代到该设备；含 WebSocket 升级
	//   GET /relay/tunnel          设备端隧道入口（WSS）
	Handler() http.Handler
	// Online 返回已连上的设备 id（运维/CLI 用）。
	Online() []string
	Close() error
}

// OpenServer 启动 relay。实现见 server.go（W1）。
func OpenServer(cfg ServerConfig) (Server, error) { return openServerImpl(cfg) }

// TunnelConfig 是设备端（跑在用户电脑上）的配置。
type TunnelConfig struct {
	URL       string // ws://127.0.0.1:9443 或 wss://relay.example.com
	Token     string
	DeviceID  string // 由配对时的设备 id 派生，或本地生成后登记
	TenantID  string
	LocalAddr string // 127.0.0.1:7788（控制面）
	Logger    *log.Logger
	// Insecure 允许跳过 TLS 校验，仅本机验证用。
	Insecure bool
}

// TunnelStats 供 /v1/remote/tunnel 与 CLI 展示。
type TunnelStats struct {
	Connected  bool      `json:"connected"`
	URL        string    `json:"url"`
	DeviceID   string    `json:"deviceId"`
	Since      time.Time `json:"since,omitempty"`
	Reconnects int       `json:"reconnects"`
	Streams    int       `json:"streams"`
	LastError  string    `json:"lastError,omitempty"`
}

// Tunnel 是设备端隧道。
type Tunnel interface {
	Stats() TunnelStats
	Close() error
}

// Dial 建立并维持隧道（内部自动重连，指数退避）。实现见 tunnel.go（W1）。
func Dial(cfg TunnelConfig) (Tunnel, error) { return dialImpl(cfg) }
