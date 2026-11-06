// Package connector 管两件事：**接哪些外部系统**、**凭据怎么保管**。
//
// 拆开看：Connector 是「怎么拿到 token」（OAuth 流程），Vault 是「token 存在哪」。
// 合成一个包是因为它们只能用在一起——单独一个 OAuth 客户端没有价值，
// 单独一个保管库不知道该存什么。
//
// 安全底线：
//   - token 落盘必须加密（AES-GCM，主密钥来自 ASTRA_VAULT_KEY，不落盘）
//   - agent 永远看不到明文 token：注入时给沙箱的是「本地代理地址 + 一次性票据」，
//     真正的 token 由代理在转发时替换（和 Muse 的凭据托管同一个思路）
//   - 这条约束在 MVP 里可以先退化成「注入环境变量」，但必须显式标记风险，
//     且默认关闭——见 InjectMode
package connector

import (
	"context"
	"errors"
	"time"
)

// Kind 是连接器的认证方式。
type Kind string

const (
	// KindOAuth2 走标准授权码流程。
	KindOAuth2 Kind = "oauth2"
	// KindAPIKey 用户直接粘贴一个 key（很多国内 SaaS 不给 OAuth）。
	KindAPIKey Kind = "apikey"
)

// Connector 是内置的连接器定义。MVP 阶段写在代码里（notion/linear/github/google），
// 不做「租户自己上传 OAuth 应用」——那需要一套审核流程，先不碰。
type Connector struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        Kind   `json:"kind"`
	// Scopes 是申请的最小权限集。不要写 "*"，用户看到授权页会直接拒。
	Scopes []string `json:"scopes,omitempty"`
	// EnvNames 是注入给沙箱的环境变量名，按顺序对应 token 字段。
	// 例：["NOTION_TOKEN"]。
	EnvNames []string `json:"env_names"`
	// DocsURL 在 CLI/前端提示「去哪拿 key」。
	DocsURL string `json:"docs_url,omitempty"`
}

// Credential 是某用户对某连接器的授权。
type Credential struct {
	TenantID string     `json:"tenant_id"`
	UserID   string     `json:"user_id"`
	Name     string     `json:"name"`
	Kind     Kind       `json:"kind"`
	Token    string     `json:"-"` // 明文只在内存里，落盘加密
	Refresh  string     `json:"-"`
	Expires  time.Time  `json:"expires,omitempty"`
	Scopes   []string   `json:"scopes,omitempty"`
	LinkedAt time.Time  `json:"linked_at"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// InjectMode 决定凭据怎么进沙箱。
type InjectMode string

const (
	// InjectEnv 直接把 token 写进沙箱环境变量。**有泄漏风险**（agent 可以读到、
	// 可以打印到日志），只建议在可信 agent + 短生命周期 token 时用。
	InjectEnv InjectMode = "env"
	// InjectProxy 给沙箱的是本地代理地址，token 由代理替换。
	// 这是正确做法，但要求出网代理可用（配合 internal/egress）。
	InjectProxy InjectMode = "proxy"
)

// Manager 是连接器能力的对外面。
type Manager interface {
	Enabled() bool
	List() []Connector
	// ListCredentials 只返回元信息，不含 token。
	ListCredentials(ctx context.Context, tenantID, userID string) ([]Credential, error)
	// AuthorizeURL 生成 OAuth 授权地址；APIKey 型返回空串（走 PutKey）。
	AuthorizeURL(ctx context.Context, tenantID, userID, name, redirectURI, state string) (string, error)
	// ParseState 从 OAuth 回调的 state 解出授权上下文。
	// state 必须自包含（签名或加密），不能只靠服务端内存——多副本部署时
	// 回调可能打到另一个副本上，纯内存 map 会直接失效。
	ParseState(state string) (tenantID, userID, name string, err error)
	// Exchange 用回调的 code 换 token 并存入 vault。
	Exchange(ctx context.Context, tenantID, userID, name, code, redirectURI string) (Credential, error)
	// PutKey 存一个 API key（KindAPIKey）。
	PutKey(ctx context.Context, tenantID, userID, name, key string) error
	Delete(ctx context.Context, tenantID, userID, name string) error
	// Inject 返回要注入沙箱的环境变量。mode 由配置决定。
	Inject(ctx context.Context, tenantID, userID string, names []string) (map[string]string, error)
}

// Config 装配。
type Config struct {
	// Enabled 关掉时 Open 返回 Disabled（连 OAuth 回调都不注册）。
	Enabled bool
	// VaultKey 32 字节主密钥（base64）。空则该能力不可用——没有密钥的保管库
	// 等于明文存 token，不如不做。
	VaultKey string
	// VaultPath 凭证文件路径，默认 ~/.astra/vault.json（0600）。
	VaultPath string
	// BaseURL 是平台对外地址，用于拼 OAuth redirect_uri。
	BaseURL string
	// InjectMode 见 InjectMode 注释，默认 proxy；代理不可用时降级 env 并告警。
	InjectMode InjectMode

	// OAuth 应用凭据。空表示该连接器没配，List 里仍然显示但不可连接。
	GoogleClientID, GoogleClientSecret string
	GitHubClientID, GitHubClientSecret string
	NotionClientID, NotionClientSecret string
	LinearClientID, LinearClientSecret string
}

var (
	ErrDisabled     = errors.New("connector: 未启用（未配置 VAULT_KEY）")
	ErrNotFound     = errors.New("connector: 未找到")
	ErrUnsupported  = errors.New("connector: 该连接器未配置 OAuth 应用凭据")
	ErrNotConnected = errors.New("connector: 该用户还没连接过")
)
