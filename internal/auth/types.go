// Package auth 负责「你是谁」和「你能干什么」。
//
// 两条边界，别越界：
//   - 这里只做身份与授权判定，不碰业务逻辑，不直接读写 sessions。
//   - 这里不关心 HTTP。中间件在 api 层，本包只给出 Principal 和判定函数，
//     这样同一套权限模型将来能被 gRPC / CLI 复用。
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 认证失败的原因分类。API 层据此决定回 401 还是 403。
var (
	// ErrNoCredential 请求没带凭据。
	ErrNoCredential = errors.New("auth: 缺少凭据")
	// ErrInvalidCredential 凭据格式不对、签名不通过、或被撤销。
	ErrInvalidCredential = errors.New("auth: 凭据无效")
	// ErrExpired 凭据过期。
	ErrExpired = errors.New("auth: 凭据已过期")
	// ErrForbidden 身份有效但无权执行该动作。
	ErrForbidden = errors.New("auth: 权限不足")
	// ErrTenantSuspended 租户被停用。
	ErrTenantSuspended = errors.New("auth: 租户已停用")
)

// Via 说明这次请求是靠什么凭据进来的。审计里要记，排障时第一个要看的就是它。
const (
	ViaJWT    = "jwt"
	ViaAPIKey = "apikey"
	ViaDev    = "dev" // 单租户兼容模式，无认证
)

// Principal 是一次请求的身份。所有 handler 都从这里取 tenantID，不许从请求体里读。
type Principal struct {
	UserID   string `json:"userId"`
	TenantID string `json:"tenantId"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Via      string `json:"via"`
	// KeyID 仅 Via==apikey 时有值，用于 TouchAPIKey。
	KeyID string `json:"keyId,omitempty"`
}

// IsZero 判断是否是「匿名 / 兼容模式」身份。
func (p Principal) IsZero() bool { return p.UserID == "" && p.TenantID == "" }

// Action 是可授权动作。用枚举而不是字符串裸传，是为了让「少写一个 case」变成编译期问题。
type Action string

const (
	ActionSessionCreate Action = "session:create"
	ActionSessionRead   Action = "session:read"
	ActionSessionDelete Action = "session:delete"
	ActionRunCreate     Action = "run:create"
	ActionRunCancel     Action = "run:cancel"
	ActionApproval      Action = "approval:resolve"
	ActionSandboxRead   Action = "sandbox:read"
	ActionSandboxManage Action = "sandbox:manage"
	ActionKeyManage     Action = "key:manage"
	ActionUserManage    Action = "user:manage"
	ActionUsageRead     Action = "usage:read"
	ActionUsageReadAll  Action = "usage:read:all" // 看全租户，不只是自己
	ActionAuditRead     Action = "audit:read"
	ActionTenantManage  Action = "tenant:manage"
	// 记忆是「用户自己的东西」，member 就有读写权；但 API 层强制按 tenant+user 过滤，
	// 所以这个权限不代表能读别人的记忆。
	ActionMemoryRead  Action = "memory:read"
	ActionMemoryWrite Action = "memory:write"
	// 观测数据含 prompt 与工具入参，只给能看全租户的角色。
	ActionObsRead Action = "obs:read"
)

// permissions 是角色到动作的白名单。刻意写成「列出允许」而不是「列出禁止」：
// 新增动作时如果忘了授权，默认是拒绝——这个方向失败才安全。
var permissions = map[string]map[Action]bool{
	store.RoleOwner: {
		ActionSessionCreate: true, ActionSessionRead: true, ActionSessionDelete: true,
		ActionRunCreate: true, ActionRunCancel: true, ActionApproval: true,
		ActionSandboxRead: true, ActionSandboxManage: true,
		ActionKeyManage: true, ActionUserManage: true,
		ActionUsageRead: true, ActionUsageReadAll: true,
		ActionAuditRead: true, ActionTenantManage: true,
		ActionMemoryRead: true, ActionMemoryWrite: true, ActionObsRead: true,
	},
	store.RoleAdmin: {
		ActionSessionCreate: true, ActionSessionRead: true, ActionSessionDelete: true,
		ActionRunCreate: true, ActionRunCancel: true, ActionApproval: true,
		ActionSandboxRead: true, ActionSandboxManage: true,
		ActionKeyManage: true, ActionUserManage: true,
		ActionUsageRead: true, ActionUsageReadAll: true,
		ActionAuditRead:  true,
		ActionMemoryRead: true, ActionMemoryWrite: true, ActionObsRead: true,
		// 没有 ActionTenantManage：管理员不能改租户本身（额度、停用、删除）。
	},
	store.RoleMember: {
		ActionSessionCreate: true, ActionSessionRead: true, ActionSessionDelete: true,
		ActionRunCreate: true, ActionRunCancel: true, ActionApproval: true,
		ActionSandboxRead: true,
		ActionUsageRead:   true, // 只能看自己的，由 API 层按 UserID 过滤
		ActionMemoryRead:  true, // 同上：API 层强制按 UserID 过滤
		ActionMemoryWrite: true,
		// 没有 KeyManage / UserManage / AuditRead / SandboxManage：成员不能管别人。
	},
}

// Can 判断该身份能否执行动作。未知角色一律拒绝。
func (p Principal) Can(a Action) bool {
	if p.Via == ViaDev {
		return true // 兼容模式：单机、无认证、全放行（启动时会打醒目告警）
	}
	m, ok := permissions[p.Role]
	if !ok {
		return false
	}
	return m[a]
}

// Require 是 Can 的报错版，handler 里直接 `if err := p.Require(x); err != nil { ... }`。
func (p Principal) Require(a Action) error {
	if !p.Can(a) {
		return ErrForbidden
	}
	return nil
}

// ── 上下文传递 ──

type ctxKey struct{}

// WithPrincipal 把身份放进 context。只在中间件里调用。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext 取出身份。第二个返回值 false 表示没经过认证中间件。
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// ── 凭据格式 ──

const (
	// APIKeyPrefix 所有平台签发的长期凭据都以它开头，便于日志脱敏时正则识别。
	APIKeyPrefix = "astra_sk_"
	// APIKeyPrefixLen 展示用前缀长度（含 astra_sk_）。
	APIKeyPrefixLen = 17
)

// Authenticator 把一串不透明 token 解析成 Principal。
//
// 这是接口而不是 struct，是为了让测试能塞假实现，不必搭真库。
type Authenticator interface {
	// Authenticate 接受 JWT 或 API Key，自动判别。
	Authenticate(ctx context.Context, token string) (Principal, error)

	// Login 校验邮箱密码，成功返回用户。
	Login(ctx context.Context, tenantSlug, email, password string) (store.User, error)

	// Issue 为已认证用户签发 JWT。
	Issue(u store.User) (token string, expiresAt time.Time, err error)
}

// Clock 让有效期相关逻辑可测。生产用 time.Now。
type Clock func() time.Time
