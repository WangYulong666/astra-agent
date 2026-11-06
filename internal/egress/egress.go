// Package egress 是沙箱的**出网中介**。
//
// 为什么必须有：沙箱目前只限制了文件写（sandbox-exec profile），网络是通的。
// 平台定位是「跑客户自传的不可信算法」，出网不控这个定位就不成立——
// 一段被投毒的代码可以把工作区内容外传，或者把沙箱当跳板打内网。
//
// 形态照 Muse 的 Sentinel：沙箱内所有 HTTP(S) 走一个本地 CONNECT 代理，
// 代理按「租户策略 + 目标域名」决定放行/拦截/送审批，并逐条记录审计。
// 注意这里只做**出口**：入站流量由网关管，不在这里。
package egress

import (
	"context"
	"errors"
	"time"
)

// Decision 是一条出网请求的处置结果。
type Decision string

const (
	// Allow 直接放行。
	Allow Decision = "allow"
	// Deny 直接拒绝（连接返回 403，客户端表现为 TLS 握手失败或 proxy error）。
	Deny Decision = "deny"
	// Ask 需要人工审批。审批通过前连接挂起，超时按 Deny 处理。
	Ask Decision = "ask"
)

// Rule 按域名匹配。Pattern 支持 `api.example.com`（精确）、`*.example.com`
// （含子域，不含 example.com 本身）、`*`（全部）。大小写不敏感。
// 端口不参与匹配：策略是「能不能连到这台主机」，不是「能不能连到某个端口」。
type Rule struct {
	Pattern  string   `json:"pattern"`
	Decision Decision `json:"decision"`
	Note     string   `json:"note,omitempty"`
}

// Policy 是租户级出网策略。规则按顺序匹配，第一条命中即生效；
// 都没命中走 Default。
type Policy struct {
	// Default 没命中任何规则时怎么办。默认 deny——宁可让用户加白名单，
	// 也不要默认放行后再来解释为什么数据出去了。
	Default Decision `json:"default"`
	Rules   []Rule   `json:"rules"`
	// AllowPrivate 是否允许连内网地址（10/8、172.16/12、192.168/16、127/8、169.254/16）。
	// 默认 false：SSRF 防护的第一道门，云元数据服务在 169.254.169.254。
	AllowPrivate bool `json:"allow_private"`
	// LogBody 是否记录请求体摘要（默认 false，只记域名和大小）。
	LogBody bool `json:"log_body"`
}

// LogEntry 是一条出网审计。
type LogEntry struct {
	Time      time.Time `json:"time"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Host      string    `json:"host"`
	Port      string    `json:"port,omitempty"`
	Decision  Decision  `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	BytesUp   int64     `json:"bytes_up,omitempty"`
	BytesDown int64     `json:"bytes_down,omitempty"`
	// ApprovalID 非空表示这条走了审批（Ask）。
	ApprovalID string `json:"approval_id,omitempty"`
}

// Config 装配代理。
type Config struct {
	// ListenAddr 代理监听地址，如 127.0.0.1:17890；空表示未启用。
	ListenAddr string
	// DefaultPolicy 新租户/未配置租户的默认策略。
	DefaultPolicy Policy
	// LogSize 内存里保留多少条审计（环形缓冲）；0 用默认 5000。
	LogSize int
	// Approver 为 nil 时 Ask 降级为 Deny（reason=ask-no-approver）。
	// 由 app 在 runner 建好后注入，见 ext.go 的依赖倒置说明。
	Approver Approver
	// TokenProvider 为 nil 时不启用凭据注入与 sidecar 端点。
	TokenProvider TokenProvider
	// AskTimeout 是一条出网审批的等待上限；0 用默认 5 分钟。
	AskTimeout time.Duration
}

// Controller 是出网能力的对外面。
//
// 沙箱拿到的是 Addr()，注入成 HTTP_PROXY / HTTPS_PROXY / ALL_PROXY；
// 代理通过 CONNECT 的 host:port 反查「这是哪个 run」——靠来源端口到
// run 的映射（Provider 建沙箱时登记），查不到就按租户默认策略处理并标 unowned。
type Controller interface {
	Enabled() bool
	// Addr 返回给沙箱用的代理地址。未启用时返回空串。
	Addr() string
	// Policy 读某租户的策略。租户没配过就返回默认值。
	Policy(ctx context.Context, tenantID string) (Policy, error)
	SetPolicy(ctx context.Context, tenantID string, p Policy) error
	// Log 返回最近的出网记录，按时间倒序。tenantID 为空表示全部（仅运营可用）。
	Log(ctx context.Context, tenantID string, limit int) ([]LogEntry, error)
	// Register 把一个沙箱登记到租户/run，代理据此归属流量。
	Register(sandboxID, tenantID, userID, sessionID, runID string)
	Unregister(sandboxID string)
	Close() error
}

// ErrDisabled 表示出网能力未启用（没配 ListenAddr）。
var ErrDisabled = errors.New("egress: 未启用（未配置 EGRESS_LISTEN_ADDR）")

// 默认策略：只放行模型与包管理所需的域名，其余拒绝。
//
// 这份清单刻意短——它不是「白名单模板」，而是「新租户开箱能用」的最小集。
// 真正上线前应该由运营按租户收紧，而不是指望这份默认值。
func DefaultPolicy() Policy {
	return Policy{
		Default: Deny,
		Rules: []Rule{
			{Pattern: "*.openai.com", Decision: Allow, Note: "模型"},
			{Pattern: "*.anthropic.com", Decision: Allow, Note: "模型"},
			{Pattern: "*.deepseek.com", Decision: Allow, Note: "模型"},
			{Pattern: "registry.npmjs.org", Decision: Allow, Note: "npm"},
			{Pattern: "pypi.org", Decision: Allow, Note: "pip"},
			{Pattern: "files.pythonhosted.org", Decision: Allow, Note: "pip"},
			{Pattern: "proxy.golang.org", Decision: Allow, Note: "go"},
			{Pattern: "goproxy.cn", Decision: Allow, Note: "go 国内镜像"},
			{Pattern: "*.githubusercontent.com", Decision: Allow, Note: "raw 文件"},
		},
	}
}
