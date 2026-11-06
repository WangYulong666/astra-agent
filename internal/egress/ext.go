package egress

import (
	"context"
	"time"
)

// ── 依赖倒置的两个窄接口 ────────────────────────────────────────
//
// egress 是零内部依赖的叶子包，而审批能力在 runner（runner 已经 import
// egress）、凭据能力在 connector。让 egress 直接 import runner 会立刻成环，
// 所以这里只声明「我需要什么」，由 app 层用 adapter 注入具体实现。

// Approver 发起一条出网审批并阻塞等到结论。
//
// 实现方（runner）会把请求登记进现有的人工审批表，键是 (runID, toolCallID)，
// 于是「取消 / 控制面退出 / 重启 expire → 自动按 deny 唤醒」这些既有逻辑
// 零改动就能覆盖挂起的连接。
type Approver interface {
	Request(ctx context.Context, req ApprovalRequest) (ApprovalResult, error)
}

type ApprovalRequest struct {
	RunID      string
	ToolCallID string
	Kind       string // 固定 "egress"
	Title      string // 例如 "允许访问 api.example.com:443"
	TenantID   string
	UserID     string
	SessionID  string
	Host       string
	Port       string
	Timeout    time.Duration
}

type ApprovalResult struct {
	// Decision 是 "allow" / "deny" / "timeout"。
	Decision   string
	ApprovalID string
}

// TokenProvider 由 connector.Manager 适配而来，token 永远不进沙箱。
type TokenProvider interface {
	// TokenFor 按目标 host 找到该用户授权的连接器并取（必要时刷新）token。
	// 返回要写入上游请求的 header 名与值。没授权时 ok=false（用 err 区分系统错误）。
	TokenFor(ctx context.Context, tenantID, userID, host string) (header, value string, ok bool, err error)
	// BaseFor 给出某连接器的上游 HTTPS 基址，供本地 sidecar 端点转发。
	BaseFor(name string) (baseURL string, ok bool)
	// NameFor 按 host 反查连接器名（sidecar 路径里的 {name} 也要校验）。
	NameFor(host string) (name string, ok bool)
}

// sidecarPath 是代理上的明文转发端点前缀。
//
// 为什么不直接改 CONNECT 隧道里的请求：HTTPS 走 CONNECT 时客户端自己跑 TLS，
// 代理只搬运密文，看不到也无法替换 Authorization。要做透明注入只能 MITM
// （自建 CA + 双层 TLS + 处理证书固定），改动与安全面都太大。
// 改由代理暴露一个 http://127.0.0.1:<proxy>/_connector/{name}/* 端点：
// agent 用明文调本机，代理查 TokenFor 注入 header 后再转发到真正的 HTTPS 上游。
// 代价是 agent 必须显式使用这个地址（connector 的 Inject 会把它作为 *_API_BASE
// 环境变量给到沙箱），对官方 SDK 需要配 base URL；好处是 token 一步都没离开控制面。
const sidecarPath = "/_connector/"
