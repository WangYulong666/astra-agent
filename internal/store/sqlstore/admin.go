package sqlstore

// 治理层存储：租户、用户、凭据、用量、审计。契约见 internal/store/admin.go。
//
// 一条总约定：所有「按租户查询」的方法在 tenantID 为空时表示跨租户（平台超管），
// 实现上就是不加这个过滤条件，不额外赋予「空值 = 查不到」之类的语义。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// normalizeEmail 统一小写去空格。
//
// 不归一化的话 Alice@x.com 和 alice@x.com 会绕过 (tenant_id, email) 唯一约束
// 变成两个账号，登录时用户根本分不清自己拿到的是哪一个。
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ── 租户 ─────────────────────────────────────────────────────

const tenantCols = `id, name, slug, status, monthly_token_limit, created_at`

func scanTenant(sc scanner) (store.Tenant, error) {
	var (
		t       store.Tenant
		created dbTime
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Slug, &t.Status, &t.MonthlyTokenLimit, &created); err != nil {
		return t, err
	}
	t.CreatedAt = created.t
	return t, nil
}

func (s *Store) CreateTenant(ctx context.Context, t store.Tenant) error {
	if t.ID == "" || t.Slug == "" {
		return errors.New("sqlstore: 租户 id 与 slug 不能为空")
	}
	if t.Status == "" {
		t.Status = store.StatusActive
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO tenants (`+tenantCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Slug, t.Status, t.MonthlyTokenLimit, s.timeVal(t.CreatedAt))
	return err
}

func (s *Store) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	t, err := scanTenant(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+tenantCols+` FROM tenants WHERE id = ?`), id))
	if err != nil {
		return store.Tenant{}, s.mapErr(err)
	}
	return t, nil
}

func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (store.Tenant, error) {
	t, err := scanTenant(s.db.QueryRowContext(ctx,
		s.rebind(`SELECT `+tenantCols+` FROM tenants WHERE slug = ?`), slug))
	if err != nil {
		return store.Tenant{}, s.mapErr(err)
	}
	return t, nil
}

func (s *Store) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	rows, err := s.query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Tenant, 0, 4)
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTenant(ctx context.Context, t store.Tenant) error {
	res, err := s.exec(ctx, `UPDATE tenants SET name = ?, slug = ?, status = ?, monthly_token_limit = ? WHERE id = ?`,
		t.Name, t.Slug, t.Status, t.MonthlyTokenLimit, t.ID)
	if err != nil {
		return err
	}
	return requireAffected(res, "租户 "+t.ID)
}

// ── 用户 ─────────────────────────────────────────────────────

const userCols = `id, tenant_id, email, display_name, password_hash, role, status, created_at, last_login_at, token_valid_after`

func scanUser(sc scanner) (store.User, error) {
	var (
		u          store.User
		created    dbTime
		lastLogin  dbTime
		validAfter dbTime
	)
	err := sc.Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.PasswordHash,
		&u.Role, &u.Status, &created, &lastLogin, &validAfter)
	if err != nil {
		return u, err
	}
	u.CreatedAt, u.LastLoginAt, u.TokenValidAfter = created.t, lastLogin.t, validAfter.t
	return u, nil
}

func (s *Store) CreateUser(ctx context.Context, u store.User) error {
	if u.ID == "" || u.TenantID == "" {
		return errors.New("sqlstore: 用户 id 与租户 id 不能为空")
	}
	u.Email = normalizeEmail(u.Email)
	if u.Status == "" {
		u.Status = store.StatusActive
	}
	if u.Role == "" {
		u.Role = store.RoleMember
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.TenantID, u.Email, u.DisplayName, u.PasswordHash, u.Role, u.Status,
		s.timeVal(u.CreatedAt), s.timeVal(u.LastLoginAt), s.timeVal(u.TokenValidAfter))
	if errors.Is(err, store.ErrDuplicate) {
		return fmt.Errorf("%w: 租户 %s 下邮箱 %s 已存在", store.ErrDuplicate, u.TenantID, u.Email)
	}
	return err
}

func (s *Store) GetUser(ctx context.Context, id string) (store.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+userCols+` FROM users WHERE id = ?`), id))
	if err != nil {
		return store.User{}, s.mapErr(err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, tenantID, email string) (store.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		s.rebind(`SELECT `+userCols+` FROM users WHERE tenant_id = ? AND email = ?`),
		tenantID, normalizeEmail(email)))
	if err != nil {
		return store.User{}, s.mapErr(err)
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context, tenantID string) ([]store.User, error) {
	q := `SELECT ` + userCols + ` FROM users`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.User, 0, 8)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser 覆盖除 created_at 以外的全部字段。
// 空 PasswordHash 会被写进库，所以调用方改角色/状态时必须带上原 hash——
// 这是接口契约的形态（整体覆盖），不是这里擅自补的合并逻辑。
func (s *Store) UpdateUser(ctx context.Context, u store.User) error {
	res, err := s.exec(ctx, `UPDATE users SET email = ?, display_name = ?, password_hash = ?,
	role = ?, status = ?, last_login_at = ?, token_valid_after = ? WHERE id = ?`,
		normalizeEmail(u.Email), u.DisplayName, u.PasswordHash, u.Role, u.Status,
		s.timeVal(u.LastLoginAt), s.timeVal(u.TokenValidAfter), u.ID)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return fmt.Errorf("%w: 租户 %s 下邮箱 %s 已存在", store.ErrDuplicate, u.TenantID, u.Email)
		}
		return err
	}
	return requireAffected(res, "用户 "+u.ID)
}

func (s *Store) CountUsers(ctx context.Context, tenantID string) (int, error) {
	q := `SELECT COUNT(*) FROM users`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	var n int
	if err := s.queryRowScan(ctx, q, args, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// ── 凭据 ─────────────────────────────────────────────────────

const apiKeyCols = `id, tenant_id, user_id, name, prefix, key_hash, created_at, last_used_at, expires_at, revoked_at`

func scanAPIKey(sc scanner) (store.APIKey, error) {
	var (
		k        store.APIKey
		created  dbTime
		lastUsed dbTime
		expires  dbTime
		revoked  dbTime
	)
	err := sc.Scan(&k.ID, &k.TenantID, &k.UserID, &k.Name, &k.Prefix, &k.KeyHash,
		&created, &lastUsed, &expires, &revoked)
	if err != nil {
		return k, err
	}
	k.CreatedAt, k.LastUsedAt, k.ExpiresAt, k.RevokedAt = created.t, lastUsed.t, expires.t, revoked.t
	return k, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, k store.APIKey) error {
	if k.ID == "" || k.TenantID == "" || k.KeyHash == "" {
		return errors.New("sqlstore: API key 的 id、租户 id、hash 不能为空")
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO apikeys (`+apiKeyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.TenantID, k.UserID, k.Name, k.Prefix, k.KeyHash,
		s.timeVal(k.CreatedAt), s.timeVal(k.LastUsedAt), s.timeVal(k.ExpiresAt), s.timeVal(k.RevokedAt))
	if errors.Is(err, store.ErrDuplicate) {
		// hash 撞车只可能是同一明文被签发两次（或上层算错了），当成冲突报给调用方
		return fmt.Errorf("%w: key hash 已存在", store.ErrDuplicate)
	}
	return err
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (store.APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		s.rebind(`SELECT `+apiKeyCols+` FROM apikeys WHERE key_hash = ?`), hash))
	if err != nil {
		return store.APIKey{}, s.mapErr(err)
	}
	return k, nil
}

func (s *Store) ListAPIKeys(ctx context.Context, tenantID string) ([]store.APIKey, error) {
	// 已撤销的也返回：管理界面要能看到「谁在什么时候撤的」，过滤交给调用方。
	q := `SELECT ` + apiKeyCols + ` FROM apikeys`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.APIKey, 0, 8)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, tenantID, id string) error {
	// tenantID 参与 WHERE 而不是查出来再比对：跨租户撤销应当直接「查无此 key」，
	// 不能因为多一次读就让越权请求有时间窗口。
	q := `UPDATE apikeys SET revoked_at = ? WHERE id = ?`
	args := []any{s.timeVal(time.Now().UTC()), id}
	if tenantID != "" {
		q += ` AND tenant_id = ?`
		args = append(args, tenantID)
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	return requireAffected(res, "API key "+id)
}

// TouchAPIKey 更新最近使用时间。找不到 key 时不报错：
// 这是鉴权路径上的旁路写，key 刚好被撤销/删除不该让一个已通过校验的请求变成 500。
func (s *Store) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.exec(ctx, `UPDATE apikeys SET last_used_at = ? WHERE id = ?`, s.timeVal(at), id)
	return err
}

// ── 用量 ─────────────────────────────────────────────────────

const usageCols = `id, tenant_id, user_id, session_id, run_id, provider, model,
	input_tokens, output_tokens, cost_micro_usd, source, created_at`

// usageCond 生成 WHERE 片段。零值字段表示不过滤，这是 UsageFilter 的契约。
func (s *Store) usageCond(f store.UsageFilter) ([]string, []any) {
	var conds []string
	var args []any
	if f.TenantID != "" {
		conds = append(conds, "tenant_id = ?")
		args = append(args, f.TenantID)
	}
	if f.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if !f.From.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, s.timeVal(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "created_at < ?")
		args = append(args, s.timeVal(f.To))
	}
	return conds, args
}

func (s *Store) InsertUsage(ctx context.Context, u store.Usage) error {
	// 用量是高频写入路径，调用方漏填 id 不该让一次记账失败
	if u.ID == "" {
		u.ID = newID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO usage (`+usageCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.TenantID, u.UserID, u.SessionID, u.RunID, u.Provider, u.Model,
		u.InputTokens, u.OutputTokens, u.CostMicroUSD, u.Source, s.timeVal(u.CreatedAt))
	return err
}

func (s *Store) ListUsage(ctx context.Context, f store.UsageFilter) ([]store.Usage, error) {
	conds, args := s.usageCond(f)
	q := `SELECT ` + usageCols + ` FROM usage`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC`

	limit := f.Limit
	if limit == 0 {
		limit = 1000 // 契约：0 表示默认上限，负值才是不限
	}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Usage, 0, 16)
	for rows.Next() {
		var (
			u       store.Usage
			created dbTime
		)
		if err := rows.Scan(&u.ID, &u.TenantID, &u.UserID, &u.SessionID, &u.RunID,
			&u.Provider, &u.Model, &u.InputTokens, &u.OutputTokens, &u.CostMicroUSD,
			&u.Source, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = created.t
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SumUsage(ctx context.Context, f store.UsageFilter) (store.UsageSum, error) {
	conds, args := s.usageCond(f)
	q := `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cost_micro_usd), 0), COUNT(*) FROM usage`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	var sum store.UsageSum
	if err := s.queryRowScan(ctx, q, args, &sum.InputTokens, &sum.OutputTokens, &sum.CostMicroUSD, &sum.Calls); err != nil {
		return store.UsageSum{}, err
	}
	return sum, nil
}

// ── 审计 ─────────────────────────────────────────────────────

const auditCols = `id, tenant_id, user_id, actor_email, action, target_type, target_id, detail, ip, created_at`

func (s *Store) InsertAudit(ctx context.Context, a store.AuditLog) error {
	if a.ID == "" {
		a.ID = newID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	// detail 为空存 NULL 而不是空串：JSON 列上的 '' 不是合法 JSON，
	// 以后想用 PG 的 jsonb 或做 detail ? 'xxx' 查询时会被这些空串卡住。
	var detail any
	if len(a.Detail) > 0 {
		detail = string(a.Detail)
	}
	_, err := s.exec(ctx, `INSERT INTO audit_logs (`+auditCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.TenantID, a.UserID, a.ActorEmail, a.Action, a.TargetType, a.TargetID,
		detail, a.IP, s.timeVal(a.CreatedAt))
	return err
}

func (s *Store) ListAudit(ctx context.Context, f store.AuditFilter) ([]store.AuditLog, error) {
	var conds []string
	var args []any
	if f.TenantID != "" {
		conds = append(conds, "tenant_id = ?")
		args = append(args, f.TenantID)
	}
	if f.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if !f.From.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, s.timeVal(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "created_at < ?")
		args = append(args, s.timeVal(f.To))
	}

	q := `SELECT ` + auditCols + ` FROM audit_logs`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC`

	limit := f.Limit
	if limit == 0 {
		limit = 1000
	}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.AuditLog, 0, 16)
	for rows.Next() {
		var (
			a       store.AuditLog
			detail  sql.NullString
			created dbTime
		)
		if err := rows.Scan(&a.ID, &a.TenantID, &a.UserID, &a.ActorEmail, &a.Action,
			&a.TargetType, &a.TargetID, &detail, &a.IP, &created); err != nil {
			return nil, err
		}
		if detail.Valid {
			a.Detail = json.RawMessage(detail.String)
		}
		a.CreatedAt = created.t
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── 会话归属 ─────────────────────────────────────────────────

func (s *Store) ListSessionsByTenant(ctx context.Context, tenantID string) ([]store.Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	// 按活跃时间倒序：管理后台要看的是「最近在用的会话」，
	// 也让 idx_sessions_tenant_active 这条索引可以直接供排序用。
	q += ` ORDER BY last_active_at DESC, id DESC`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return collectSessions(rows)
}

// requireAffected 把「UPDATE/DELETE 命中 0 行」翻译成 ErrNotFound。
// 单独抽出来是因为 PG 和 SQLite 都不会因为更新到 0 行而报错。
func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", store.ErrNotFound, what)
	}
	return nil
}
