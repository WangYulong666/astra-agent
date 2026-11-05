package api

// 管理面 handler：租户 / 用户 / API Key / 审计。
//
// 归属：T3 实现。方法签名是契约，不要改。
//
// 三条贯穿全文件的安全约定（每处都按它写，不再重复注释）：
//   - tenantID 只从 Principal 取。请求体/query 里出现 tenantId 一律忽略——
//     接受它等于把「能不能跨租户」交给客户端决定。
//   - 目标对象不属于本租户时回 404。403 会告诉对方「这个 ID 存在」，
//     把「租户 A 有多少用户」这类信息白送出去。
//   - 创建/更新接口回体里不能出现任何明文凭据，唯一例外是「刚生成、且只出现一次」
//     的初始密码与 API Key 明文。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 租户 ─────────────────────────────────────────────────────

// canSeeTenant 判断能否读/改这个租户。
//
// tenant:manage 是 owner 的权限，但 owner 是「本租户的所有者」，不是平台超管。
// 让他列出全部租户的名字和额度，就是跨租户信息泄漏。
// 只有 ViaDev（单机兼容模式）没有租户概念，才放行全部。
func (h *handler) canSeeTenant(p auth.Principal, id string) bool {
	if p.Via == auth.ViaDev {
		return true
	}
	return id != "" && id == p.TenantID
}

func (h *handler) listTenants(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	ts, err := h.cfg.Admin.ListTenants(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	out := make([]store.Tenant, 0, len(ts))
	for _, t := range ts {
		if h.canSeeTenant(p, t.ID) {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) createTenant(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	var body struct {
		Name              string `json:"name"`
		Slug              string `json:"slug"`
		MonthlyTokenLimit int64  `json:"monthlyTokenLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	slug := normalizeSlug(body.Slug)
	if name == "" {
		fail(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if !validSlug(slug) {
		fail(w, http.StatusBadRequest, "slug 只能是小写字母、数字和连字符，3~32 位，且不能以连字符开头或结尾")
		return
	}
	if body.MonthlyTokenLimit < 0 {
		fail(w, http.StatusBadRequest, "monthlyTokenLimit 不能为负数（0 表示不限）")
		return
	}
	id, err := auth.RandomID()
	if err != nil {
		h.failErr(w, err)
		return
	}
	t := store.Tenant{
		ID:                id,
		Name:              name,
		Slug:              slug,
		Status:            store.StatusActive,
		MonthlyTokenLimit: body.MonthlyTokenLimit,
		CreatedAt:         time.Now().UTC(),
	}
	if err := h.cfg.Admin.CreateTenant(r.Context(), t); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			fail(w, http.StatusConflict, "租户标识已被占用")
			return
		}
		h.failErr(w, err)
		return
	}
	// 审计落在调用者自己的租户下，target 是新租户：
	// 「A 租户的 owner 建了 B 租户」这件事必须查得到，否则谁开的租户没人知道。
	h.audit(r, "tenant.create", "tenant", t.ID, map[string]string{"slug": t.Slug})
	writeJSON(w, http.StatusCreated, t)
}

func (h *handler) getTenant(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	if !h.canSeeTenant(p, id) {
		fail(w, http.StatusNotFound, "租户不存在")
		return
	}
	t, err := h.cfg.Admin.GetTenant(r.Context(), id)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// updateTenant 只允许改 name / status / monthlyTokenLimit。
//
// slug 不给改：它是登录入口（用户每天输入的就是它），改了之后所有已发出的
// 脚本、文档、书签全部失效，收益却只有「好看一点」。
// id / createdAt 更不用说，它们是身份，不是属性。
func (h *handler) updateTenant(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	if !h.canSeeTenant(p, id) {
		fail(w, http.StatusNotFound, "租户不存在")
		return
	}
	var body struct {
		Name              *string `json:"name"`
		Status            *string `json:"status"`
		MonthlyTokenLimit *int64  `json:"monthlyTokenLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	t, err := h.cfg.Admin.GetTenant(r.Context(), id)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			fail(w, http.StatusBadRequest, "name 不能为空")
			return
		}
		t.Name = name
	}
	if body.Status != nil {
		st := strings.TrimSpace(*body.Status)
		if st != store.StatusActive && st != store.StatusSuspended {
			fail(w, http.StatusBadRequest, "status 只能是 active 或 suspended")
			return
		}
		t.Status = st
	}
	if body.MonthlyTokenLimit != nil {
		if *body.MonthlyTokenLimit < 0 {
			fail(w, http.StatusBadRequest, "monthlyTokenLimit 不能为负数（0 表示不限）")
			return
		}
		t.MonthlyTokenLimit = *body.MonthlyTokenLimit
	}
	if err := h.cfg.Admin.UpdateTenant(r.Context(), t); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "tenant.update", "tenant", t.ID, map[string]any{
		"name":              t.Name,
		"status":            t.Status,
		"monthlyTokenLimit": t.MonthlyTokenLimit,
	})
	writeJSON(w, http.StatusOK, t)
}

// ── 用户 ─────────────────────────────────────────────────────

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	us, err := h.cfg.Admin.ListUsers(r.Context(), p.TenantID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if us == nil {
		us = []store.User{}
	}
	// User.PasswordHash 的 json tag 是 "-"，所以这里不会漏；
	// 真正的风险在于有人给那个字段改 tag，改的时候请想一遍这个接口。
	writeJSON(w, http.StatusOK, us)
}

func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	if p.TenantID == "" {
		fail(w, http.StatusBadRequest, "当前没有租户上下文，无法创建用户")
		return
	}
	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
		Password    string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	email := normalizeEmail(body.Email)
	if !validEmail(email) {
		fail(w, http.StatusBadRequest, "email 格式不正确")
		return
	}
	role := strings.ToLower(strings.TrimSpace(body.Role))
	if role == "" {
		role = store.RoleMember // 缺省最小权限：不填就是普通成员，不是管理员
	}
	if !validRole(role) {
		fail(w, http.StatusBadRequest, "role 只能是 owner / admin / member")
		return
	}
	// 提权检查：admin 能管用户，但不能再造一个 owner（owner 能停用租户、改额度）。
	// 否则「管理员」和「所有者」就没有区别了，出事时追责也追不清。
	if role == store.RoleOwner && p.Role != store.RoleOwner {
		fail(w, http.StatusForbidden, "只有 owner 可以创建 owner")
		return
	}

	password := body.Password
	generated := false
	if password == "" {
		var err error
		password, err = auth.GeneratePassword()
		if err != nil {
			h.failErr(w, err)
			return
		}
		generated = true
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := auth.RandomID()
	if err != nil {
		h.failErr(w, err)
		return
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		// 兜底用邮箱前缀：列表里只有一个邮箱时，界面会挤成一行且分不清谁是谁。
		if at := strings.IndexByte(email, '@'); at > 0 {
			displayName = email[:at]
		} else {
			displayName = email
		}
	}
	u := store.User{
		ID:           id,
		TenantID:     p.TenantID,
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: hash,
		Role:         role,
		Status:       store.StatusActive,
		CreatedAt:    time.Now().UTC(),
	}
	if err := h.cfg.Admin.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			fail(w, http.StatusConflict, "该邮箱在本租户下已存在")
			return
		}
		h.failErr(w, err)
		return
	}
	// 审计里不能放密码，只说「是否由系统生成」——这条记录会被很多人看到。
	h.audit(r, "user.create", "user", u.ID, map[string]any{"email": u.Email, "role": u.Role, "generatedPassword": generated})

	resp := map[string]any{"user": u}
	if generated {
		// 明文只在这里出现一次：它没有存库，也没有别的接口能再取回。
		resp["initialPassword"] = password
	}
	writeJSON(w, http.StatusCreated, resp)
}

// updateUser 改角色 / 状态 / 显示名 / 重置密码。
//
// 这里最硬的一条：不能把租户的最后一个 owner 降级或停用。
// owner 是唯一能管租户本身的角色，把他拿掉之后，这个租户就再也没人能
// 改额度、解封、加管理员——只能进数据库手改，等于半永久锁死。
func (h *handler) updateUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	u, err := h.cfg.Admin.GetUser(r.Context(), id)
	if err != nil {
		h.failErr(w, err)
		return
	}
	// 归属校验：跨租户的用户 id 一律当不存在。
	if u.TenantID != p.TenantID && p.Via != auth.ViaDev {
		fail(w, http.StatusNotFound, "用户不存在")
		return
	}

	var body struct {
		DisplayName *string `json:"displayName"`
		Role        *string `json:"role"`
		Status      *string `json:"status"`
		Password    *string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	// 先算出「改完之后」的 role/status，再统一做两条策略检查，
	// 否则「同时把 owner 降级又停用」这种组合会各走各的检查而漏掉。
	newRole, newStatus := u.Role, u.Status
	if body.Role != nil {
		newRole = strings.ToLower(strings.TrimSpace(*body.Role))
		if !validRole(newRole) {
			fail(w, http.StatusBadRequest, "role 只能是 owner / admin / member")
			return
		}
		if newRole == store.RoleOwner && p.Role != store.RoleOwner {
			fail(w, http.StatusForbidden, "只有 owner 可以授予 owner")
			return
		}
	}
	if body.Status != nil {
		newStatus = strings.TrimSpace(*body.Status)
		if newStatus != store.StatusActive && newStatus != store.StatusSuspended {
			fail(w, http.StatusBadRequest, "status 只能是 active 或 suspended")
			return
		}
	}

	wasActiveOwner := u.Role == store.RoleOwner && u.Status == store.StatusActive
	stillActiveOwner := newRole == store.RoleOwner && newStatus == store.StatusActive
	if wasActiveOwner && !stillActiveOwner {
		n, err := h.countActiveOwners(r.Context(), u.TenantID)
		if err != nil {
			h.failErr(w, err)
			return
		}
		if n <= 1 {
			fail(w, http.StatusConflict, "不能降级或停用租户的最后一个 owner")
			return
		}
	}

	if body.DisplayName != nil {
		name := strings.TrimSpace(*body.DisplayName)
		if name == "" {
			fail(w, http.StatusBadRequest, "displayName 不能为空")
			return
		}
		u.DisplayName = name
	}
	u.Role, u.Status = newRole, newStatus

	// 管理员重置密码走这里；不返回明文——要传给别人，让管理员自己定一个新密码。
	// 与改密码同理：重置之后旧 token（含 refresh）必须全部作废，
	// 否则「重置密码」赶不走一个已经丢失的登录态。
	passwordReset := false
	if body.Password != nil && *body.Password != "" {
		hash, err := auth.HashPassword(*body.Password)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		u.PasswordHash = hash
		u.TokenValidAfter = time.Now().UTC()
		passwordReset = true
	}

	if err := h.cfg.Admin.UpdateUser(r.Context(), u); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "user.update", "user", u.ID, map[string]any{
		"role":          u.Role,
		"status":        u.Status,
		"passwordReset": passwordReset,
	})
	writeJSON(w, http.StatusOK, u)
}

// countActiveOwners 数本租户还活着的 owner。
//
// 用 ListUsers 在内存里数，而不是加一个 CountOwners：AdminStore 是冻结的契约，
// 而一个租户的用户数在几十的量级，多一次精确查询换不来什么。
func (h *handler) countActiveOwners(ctx context.Context, tenantID string) (int, error) {
	us, err := h.cfg.Admin.ListUsers(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range us {
		if u.Role == store.RoleOwner && u.Status == store.StatusActive {
			n++
		}
	}
	return n, nil
}

// ── API Key ──────────────────────────────────────────────────

func (h *handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	ks, err := h.cfg.Admin.ListAPIKeys(r.Context(), p.TenantID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if ks == nil {
		ks = []store.APIKey{}
	}
	// KeyHash 用 json:"-" 挡住；列表里只留 Prefix（前 17 位），够用户认出是哪把钥匙。
	writeJSON(w, http.StatusOK, ks)
}

func (h *handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	if p.TenantID == "" {
		fail(w, http.StatusBadRequest, "当前没有租户上下文，无法签发凭据")
		return
	}
	var body struct {
		Name string `json:"name"`
		// UserID 省略时挂在调用者自己名下。允许显式指定是为了给机器人账号发 key，
		// 但目标用户必须属于本租户——否则我们能给别人的租户造一把能用的钥匙。
		UserID        string `json:"userId"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(w, http.StatusBadRequest, "name 不能为空（用于在列表里区分是哪把钥匙）")
		return
	}
	ownerID := strings.TrimSpace(body.UserID)
	if ownerID == "" {
		ownerID = p.UserID
	}
	if ownerID == "" {
		fail(w, http.StatusBadRequest, "缺少 userId")
		return
	}
	owner, err := h.cfg.Admin.GetUser(r.Context(), ownerID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if owner.TenantID != p.TenantID {
		fail(w, http.StatusNotFound, "用户不存在")
		return
	}
	if body.ExpiresInDays < 0 {
		fail(w, http.StatusBadRequest, "expiresInDays 不能为负数")
		return
	}

	plain, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		h.failErr(w, err)
		return
	}
	id, err := auth.RandomID()
	if err != nil {
		h.failErr(w, err)
		return
	}
	now := time.Now().UTC()
	k := store.APIKey{
		ID:        id,
		TenantID:  p.TenantID,
		UserID:    owner.ID,
		Name:      name,
		Prefix:    prefix,
		KeyHash:   hash,
		CreatedAt: now,
	}
	if body.ExpiresInDays > 0 {
		k.ExpiresAt = now.Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour)
	}
	if err := h.cfg.Admin.CreateAPIKey(r.Context(), k); err != nil {
		h.failErr(w, err)
		return
	}
	// 审计只记 prefix：审计日志的读者远多于持钥匙的人。
	h.audit(r, "apikey.create", "apikey", k.ID, map[string]string{"name": k.Name, "prefix": k.Prefix, "userId": k.UserID})

	// 明文只在这个响应里出现一次。之后任何接口（包括列表）都只能看到 Prefix。
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":       k,
		"plaintext": plain,
	})
}

func (h *handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	id := r.PathValue("id")
	// tenantID 一起传下去：存储层据此保证「撤销的 key 是本租户的」，
	// 而不是靠调用方先查一遍再撤（那中间有 TOCTOU 窗口）。
	if err := h.cfg.Admin.RevokeAPIKey(r.Context(), p.TenantID, id); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "apikey.revoke", "apikey", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ── 审计 ─────────────────────────────────────────────────────

func (h *handler) listAudit(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	q := r.URL.Query()
	f := store.AuditFilter{
		TenantID: p.TenantID,
		Action:   strings.TrimSpace(q.Get("action")),
		Limit:    parseLimit(r, 100, 1000),
	}
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		fail(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		fail(w, http.StatusBadRequest, "to: "+err.Error())
		return
	}
	f.From, f.To = from, to

	logs, err := h.cfg.Admin.ListAudit(r.Context(), f)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if logs == nil {
		logs = []store.AuditLog{}
	}
	writeJSON(w, http.StatusOK, logs)
}

// ── 参数 ─────────────────────────────────────────────────────

// parseTimeParam 接受 RFC3339 与纯日期。
//
// 纯日期按 UTC 当天 0 点解释：存储层存的是 UTC，若按本地时区解释，
// 「今天」在两个时区的运维眼里会指向不同的 24 小时，对账时会对不上。
func parseTimeParam(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errors.New("时间格式应为 RFC3339 或 2006-01-02")
}

// parseLimit 读 ?limit=。非法值退回默认值而不是报错：
// 列表接口因为一个拼错的分页参数直接 400，前端会白屏。
func parseLimit(r *http.Request, def, max int) int {
	v := strings.TrimSpace(r.URL.Query().Get("limit"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// validRole 只认三个角色。未知角色在 auth.Can 里本来就会被拒，
// 但放一个脏值进库会让「这个人到底该看到什么」变成运行期才暴露的问题。
func validRole(r string) bool {
	switch r {
	case store.RoleOwner, store.RoleAdmin, store.RoleMember:
		return true
	}
	return false
}
