package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// 运营侧接口。全部走 /v1/admin/*（沿用已有 store.AdminStore 能力），
// 权限由服务端按角色判定——客户端不做「这个按钮该不该显示」的假设，
// 越权时拿到的是 403 而不是一段被前端藏起来的功能。

// AdminTenants 列租户。
func (c *Client) AdminTenants(ctx context.Context) ([]Tenant, error) {
	var out []Tenant
	if err := c.do(ctx, http.MethodGet, "/v1/admin/tenants", nil, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Tenant{}
	}
	return out, nil
}

// AdminTenant 取单个租户。
func (c *Client) AdminTenant(ctx context.Context, id string) (Tenant, error) {
	var out Tenant
	err := c.do(ctx, http.MethodGet, "/v1/admin/tenants/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// AdminCreateTenant 建租户。
func (c *Client) AdminCreateTenant(ctx context.Context, name, slug string, monthlyTokenLimit int64) (Tenant, error) {
	body := map[string]any{"name": name, "slug": slug}
	if monthlyTokenLimit > 0 {
		body["monthlyTokenLimit"] = monthlyTokenLimit
	}
	var out Tenant
	err := c.do(ctx, http.MethodPost, "/v1/admin/tenants", nil, body, &out)
	return out, err
}

// AdminSetTenantStatus 启停租户。status 只能是 active / suspended。
func (c *Client) AdminSetTenantStatus(ctx context.Context, id, status string) (Tenant, error) {
	switch status {
	case "active", "suspended":
	default:
		return Tenant{}, &UseError{Msg: "status 只能是 active 或 suspended，收到 " + status}
	}
	var out Tenant
	err := c.do(ctx, http.MethodPatch, "/v1/admin/tenants/"+url.PathEscape(id), nil,
		map[string]any{"status": status}, &out)
	return out, err
}

// AdminSetTenantQuota 改月度 token 上限。0 表示不限。
func (c *Client) AdminSetTenantQuota(ctx context.Context, id string, limit int64) (Tenant, error) {
	if limit < 0 {
		return Tenant{}, &UseError{Msg: "月度 token 上限不能为负数（0 表示不限）"}
	}
	var out Tenant
	err := c.do(ctx, http.MethodPatch, "/v1/admin/tenants/"+url.PathEscape(id), nil,
		map[string]any{"monthlyTokenLimit": limit}, &out)
	return out, err
}

// AdminUsers 列当前身份所在租户的用户。
//
// 注意：服务端只用 principal 的 tenant 过滤，没有任何「列出别的租户用户」的参数。
// 这是设计使然（防横向越权），不是缺功能——所以这里也不提供 tenantID 入参。
func (c *Client) AdminUsers(ctx context.Context) ([]User, error) {
	var out []User
	if err := c.do(ctx, http.MethodGet, "/v1/admin/users", nil, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []User{}
	}
	return out, nil
}

// NewUser 是建用户的响应：initialPassword 只在密码由系统生成时出现一次。
type NewUser struct {
	User            User   `json:"user"`
	InitialPassword string `json:"initialPassword,omitempty"`
}

// AdminCreateUser 建用户。password 为空时服务端生成一个并只在响应里返回一次。
func (c *Client) AdminCreateUser(ctx context.Context, email, displayName, role, password string) (NewUser, error) {
	body := map[string]any{"email": email}
	if displayName != "" {
		body["displayName"] = displayName
	}
	if role != "" {
		body["role"] = role
	}
	if password != "" {
		body["password"] = password
	}
	var out NewUser
	err := c.do(ctx, http.MethodPost, "/v1/admin/users", nil, body, &out)
	return out, err
}

// AdminResetPassword 重置某个用户的密码。
// 服务端同时会推进 TokenValidAfter，旧 token 立即失效。
func (c *Client) AdminResetPassword(ctx context.Context, userID, newPassword string) (User, error) {
	var out User
	err := c.do(ctx, http.MethodPatch, "/v1/admin/users/"+url.PathEscape(userID), nil,
		map[string]any{"password": newPassword}, &out)
	return out, err
}

// AdminKeys 列当前租户的长期凭据（只有 prefix，没有明文）。
func (c *Client) AdminKeys(ctx context.Context) ([]APIKey, error) {
	var out []APIKey
	if err := c.do(ctx, http.MethodGet, "/v1/admin/apikeys", nil, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []APIKey{}
	}
	return out, nil
}

// AdminCreateKey 签发新凭据。明文只在返回的 NewKey.Plaintext 里出现一次。
func (c *Client) AdminCreateKey(ctx context.Context, name string, expiresInDays int) (NewKey, error) {
	body := map[string]any{"name": name}
	if expiresInDays > 0 {
		body["expiresInDays"] = expiresInDays
	}
	var out NewKey
	err := c.do(ctx, http.MethodPost, "/v1/admin/apikeys", nil, body, &out)
	return out, err
}

// AdminRevokeKey 撤销凭据。
func (c *Client) AdminRevokeKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/admin/apikeys/"+url.PathEscape(id), nil, nil, nil)
}

// AdminSandboxes 列沙箱。
func (c *Client) AdminSandboxes(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Sandbox{}
	}
	return out, nil
}

// AdminKillSandbox 强杀沙箱（不删会话记录）。
func (c *Client) AdminKillSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil, nil)
}

// AuditQuery 审计查询条件。
//
// Offset 是**客户端分页**：服务端只支持 limit，没有 offset。这里用
// 「多取 Offset+Limit 条再切掉前 Offset 条」实现翻页——对审计这种
// 「按时间倒序、只需要往后翻几页」的读法足够，也不给服务端加接口。
type AuditQuery struct {
	Action string
	// From / To 支持 RFC3339 或纯日期（YYYY-MM-DD）。
	From   string
	To     string
	Limit  int
	Offset int
}

// AdminAudit 查审计。
func (c *Client) AdminAudit(ctx context.Context, q AuditQuery) ([]AuditLog, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit+q.Offset))
	if q.Action != "" {
		params.Set("action", q.Action)
	}
	if q.From != "" {
		params.Set("from", q.From)
	}
	if q.To != "" {
		params.Set("to", q.To)
	}
	var out []AuditLog
	if err := c.do(ctx, http.MethodGet, "/v1/admin/audit", params, nil, &out); err != nil {
		return nil, err
	}
	if q.Offset > 0 {
		if q.Offset >= len(out) {
			return []AuditLog{}, nil
		}
		out = out[q.Offset:]
	}
	if out == nil {
		out = []AuditLog{}
	}
	return out, nil
}

// UsageQuery 用量查询条件。
type UsageQuery struct {
	UserID string
	// From / To 支持 RFC3339 或纯日期。
	From  string
	To    string
	Limit int
}

// AdminUsage 查本租户用量（明细 + 全时段汇总）。
func (c *Client) AdminUsage(ctx context.Context, q UsageQuery) (UsageReport, error) {
	params := url.Values{}
	if q.UserID != "" {
		params.Set("userId", q.UserID)
	}
	if q.From != "" {
		params.Set("from", q.From)
	}
	if q.To != "" {
		params.Set("to", q.To)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out UsageReport
	err := c.do(ctx, http.MethodGet, "/v1/admin/usage", params, nil, &out)
	return out, err
}

// ObsConfig 观测能力状态。
func (c *Client) ObsConfig(ctx context.Context) (ObsConfig, error) {
	var out ObsConfig
	err := c.do(ctx, http.MethodGet, "/v1/admin/obs/config", nil, nil, &out)
	return out, err
}

// ObsTraces 反查 trace（服务端持 secret key）。
func (c *Client) ObsTraces(ctx context.Context, runID string, limit int) ([]Trace, error) {
	if limit <= 0 {
		limit = 20
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	if runID != "" {
		params.Set("run_id", runID)
	}
	var out struct {
		Items []Trace `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/admin/obs/traces", params, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Trace{}
	}
	return out.Items, nil
}

// UseError 是纯客户端侧的用法错误（参数不合法、缺必填项）。
// 单独一个类型是为了让 CLI 能把它映射成退出码 2（用法错误），
// 而不是混在「服务端拒绝了」里一起返回 1。
type UseError struct{ Msg string }

func (e *UseError) Error() string { return e.Msg }

// IsUseError 判断 err 是否是用法错误。
func IsUseError(err error) bool {
	var ue *UseError
	return errors.As(err, &ue)
}
