package store

// 平台治理层实体与接口。
//
// 与 types.go 的分界：那边是「运行时」——会话、run、事件，一次请求的生命周期；
// 这边是「治理」——租户、用户、凭据、用量、审计，跨会话长期存在。
// 刻意分成两个接口而不是塞进 Store：file store 可以只实现运行时部分（本地调试够用），
// SQL store 必须两个都实现。

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 角色。数值不代表权限大小，只表示枚举顺序；鉴权走 Can() 而不是比大小。
const (
	RoleOwner  = "owner"  // 租户所有者：全部权限，含删租户、转移所有权
	RoleAdmin  = "admin"  // 管理员：管用户、管凭据、管沙箱、看全部用量
	RoleMember = "member" // 成员：只能用自己的会话，看不到别人的
)

// 账号状态。
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
)

// ErrUnauthorized 凭据无效/过期/被撤销。调用方应回 401 而不是 500。
var ErrUnauthorized = errors.New("store: unauthorized")

// ErrDuplicate 唯一约束冲突（邮箱、slug 重复）。
var ErrDuplicate = errors.New("store: duplicate")

// Tenant 租户。所有业务数据都挂在租户下，跨租户不可见。
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"` // 登录用短名，全局唯一
	// Status active / suspended。suspended 的租户所有请求 403。
	Status string `json:"status"`
	// MonthlyTokenLimit 每月 token 上限，0 表示不限。超限后拒绝新 run。
	MonthlyTokenLimit int64     `json:"monthlyTokenLimit"`
	CreatedAt         time.Time `json:"createdAt"`
}

// User 用户。邮箱在租户内唯一（不是全局——同一邮箱可以是多个租户的成员）。
type User struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenantId"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName"`
	PasswordHash string    `json:"-"` // 永远不出 API
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	LastLoginAt  time.Time `json:"lastLoginAt,omitempty"`
	// TokenValidAfter 整体失效凭据的时间戳：iat 不严格晚于它的 token 一律拒绝。
	// 改密码 / 管理员重置密码时更新，是「不维护黑名单」的吊销方案。
	// 零值表示不限制。json:"-"：这是服务端判定字段，不出任何 API。
	TokenValidAfter time.Time `json:"-"`
}

// APIKey 长期凭据。明文只在创建时返回一次，库里只有 sha256。
//
// 为什么存 Prefix：用户需要在列表里分辨「这是哪个 key」，但绝不能展示明文。
// 前 12 位足够区分，泄漏了也不足以使用。
type APIKey struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenantId"`
	UserID     string    `json:"userId"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"` // 形如 astra_sk_a1b2c3
	KeyHash    string    `json:"-"`      // sha256(明文) hex，永不出 API
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	RevokedAt  time.Time `json:"revokedAt,omitempty"`
}

// Revoked 判断是否已失效。过期与撤销都算。
func (k APIKey) Revoked() bool {
	if !k.RevokedAt.IsZero() {
		return true
	}
	return !k.ExpiresAt.IsZero() && time.Now().After(k.ExpiresAt)
}

// Usage 一次模型调用的用量。
//
// 为什么有 Source：LLM 网关能精确记账（source=gateway），
// 而从 agent 上报里估算的只能记 source=agent（可能有偏差）。
// 计费只认 gateway，agent 来源仅用于展示。
type Usage struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenantId"`
	UserID       string `json:"userId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	RunID        string `json:"runId,omitempty"`
	Provider     string `json:"provider"` // deepseek / openai / ...
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	// CostMicroUSD 微美元（1e-6 USD）整数存储，避免浮点累加误差。
	// 1 美元 = 1,000,000。
	CostMicroUSD int64     `json:"costMicroUsd"`
	Source       string    `json:"source"` // gateway / agent
	CreatedAt    time.Time `json:"createdAt"`
}

// TotalTokens 入+出。
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// UsageFilter 查询条件。零值字段表示不限。
type UsageFilter struct {
	TenantID  string
	UserID    string
	SessionID string
	From      time.Time
	To        time.Time
	// Limit 0 表示默认 1000，负值表示不限。
	Limit int
}

// UsageSum 汇总结果。
type UsageSum struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	CostMicroUSD int64 `json:"costMicroUsd"`
	Calls        int64 `json:"calls"`
}

// AuditLog 审计。凡是「改变了系统状态或读取了敏感数据」的动作都要落一条。
//
// 不做「自动记录所有 HTTP 请求」：那样日志量会淹没真正的事件。
type AuditLog struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	UserID     string          `json:"userId,omitempty"`
	ActorEmail string          `json:"actorEmail,omitempty"`
	Action     string          `json:"action"`     // 如 session.create / apikey.revoke / approval.resolve
	TargetType string          `json:"targetType"` // session / user / apikey / sandbox
	TargetID   string          `json:"targetId,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	IP         string          `json:"ip,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// AuditFilter 查询条件。
type AuditFilter struct {
	TenantID string
	UserID   string
	Action   string
	From     time.Time
	To       time.Time
	Limit    int
}

// AdminStore 治理层接口。所有方法都要能在多 goroutine 下并发调用。
//
// 约定：所有按 tenant 查询的方法，tenantID 为空表示「跨租户」（仅平台超管场景），
// 实现不得把空值当成「不过滤」以外的语义。
type AdminStore interface {
	// ── 租户 ──
	CreateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	GetTenantBySlug(ctx context.Context, slug string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)
	UpdateTenant(ctx context.Context, t Tenant) error

	// ── 用户 ──
	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id string) (User, error)
	GetUserByEmail(ctx context.Context, tenantID, email string) (User, error)
	ListUsers(ctx context.Context, tenantID string) ([]User, error)
	UpdateUser(ctx context.Context, u User) error
	CountUsers(ctx context.Context, tenantID string) (int, error)

	// ── 凭据 ──
	CreateAPIKey(ctx context.Context, k APIKey) error
	GetAPIKeyByHash(ctx context.Context, hash string) (APIKey, error)
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, tenantID, id string) error
	TouchAPIKey(ctx context.Context, id string, at time.Time) error

	// ── 用量 ──
	InsertUsage(ctx context.Context, u Usage) error
	ListUsage(ctx context.Context, f UsageFilter) ([]Usage, error)
	SumUsage(ctx context.Context, f UsageFilter) (UsageSum, error)

	// ── 审计 ──
	InsertAudit(ctx context.Context, a AuditLog) error
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditLog, error)

	// ── 会话归属 ──
	// ListSessionsByTenant 按租户过滤会话。member 角色还要在 API 层再按 UserID 过滤。
	ListSessionsByTenant(ctx context.Context, tenantID string) ([]Session, error)
}
