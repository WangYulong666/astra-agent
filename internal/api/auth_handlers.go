package api

// 身份相关 handler：bootstrap / login / logout / me / password。
//
// 归属：T2 认证 + T3 管理 API 交界处，由 T3 实现。
// 这里的方法签名是契约，实现必须用它。
//
// 这一组只处理「人」：证明你是你（login）、证明你还没走（me）、改自己的密码（password）。
// 租户 / 用户的增删改在 admin_handlers.go——那组只有 owner/admin 能进，
// 和这组混在一起，review 时最容易漏掉一处越权。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// bootstrapMu 串行化首次初始化。
//
// 「系统里没有租户才允许初始化」是先查后写，两条并发请求会同时通过检查，
// 最后得到两个租户——而后面所有租户隔离都建立在「只有一个」的前提上。
// 进程内的锁只能挡单副本（多副本要靠 tenants.slug 的唯一索引兜底），
// 但 bootstrap 只在首次启动时发生一次，这个覆盖够用。
var bootstrapMu sync.Mutex

// bootstrap 是首次初始化的唯一入口：建一个租户 + 它唯一的 owner。
//
// 为什么必须回 409 而不是「已经初始化过就顺便登录」：
// 这个接口挂在公网上且不需要凭据，一旦允许重复调用，它就是一个
// 「任何人都能注册」的洞。所以判定依据只能是「系统里还没有任何租户」，
// 而不能是任何一个请求里的字段。
func (h *handler) bootstrap(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	var body struct {
		TenantName string `json:"tenantName"`
		Slug       string `json:"slug"`
		Email      string `json:"email"`
		Password   string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	name := strings.TrimSpace(body.TenantName)
	slug := normalizeSlug(body.Slug)
	email := normalizeEmail(body.Email)

	// 先校验、先算哈希，再落第一条记录：AdminStore 没有 DeleteTenant，
	// 写到一半失败只会留下一个没有 owner 的空租户，而且那个空租户会让
	// 后面的每一次 bootstrap 都返回 409——等于把系统锁死。
	if name == "" {
		fail(w, http.StatusBadRequest, "tenantName 不能为空")
		return
	}
	if !validSlug(slug) {
		fail(w, http.StatusBadRequest, "slug 只能是小写字母、数字和连字符，3~32 位，且不能以连字符开头或结尾")
		return
	}
	if !validEmail(email) {
		fail(w, http.StatusBadRequest, "email 格式不正确")
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	tenantID, err := auth.RandomID()
	if err != nil {
		h.failErr(w, err)
		return
	}
	userID, err := auth.RandomID()
	if err != nil {
		h.failErr(w, err)
		return
	}

	now := time.Now().UTC()
	tenant := store.Tenant{
		ID:        tenantID,
		Name:      name,
		Slug:      slug,
		Status:    store.StatusActive,
		CreatedAt: now,
	}
	owner := store.User{
		ID:           userID,
		TenantID:     tenantID,
		Email:        email,
		DisplayName:  name,
		PasswordHash: hash,
		Role:         store.RoleOwner,
		Status:       store.StatusActive,
		CreatedAt:    now,
	}

	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()

	existing, err := h.cfg.Admin.ListTenants(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	if len(existing) > 0 {
		fail(w, http.StatusConflict, "系统已初始化，请直接登录")
		return
	}

	if err := h.cfg.Admin.CreateTenant(r.Context(), tenant); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			fail(w, http.StatusConflict, "租户标识已被占用")
			return
		}
		h.failErr(w, err)
		return
	}
	if err := h.cfg.Admin.CreateUser(r.Context(), owner); err != nil {
		// 租户已经落库且删不掉，这里只能如实报错，让运维去库里清。
		// 静默成功更糟：用户会以为账号建好了，然后登录一直 401。
		h.log.Printf("bootstrap: 租户 %s 已创建但 owner 创建失败: %v", tenant.ID, err)
		if errors.Is(err, store.ErrDuplicate) {
			fail(w, http.StatusConflict, "该邮箱已被占用")
			return
		}
		h.failErr(w, err)
		return
	}

	// 请求本身是公开路由，没有 Principal；临时注入一份，让审计落到这个租户名下。
	// 否则「谁在什么时候初始化了系统」会记在空租户里，事后根本查不到。
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
		UserID:   owner.ID,
		TenantID: tenant.ID,
		Email:    owner.Email,
		Role:     owner.Role,
		Via:      auth.ViaJWT,
	}))
	h.audit(r, "tenant.bootstrap", "tenant", tenant.ID, map[string]string{"slug": tenant.Slug})

	writeJSON(w, http.StatusCreated, map[string]any{"tenant": tenant, "user": owner})
}

// login 校验邮箱密码并签发 JWT。
//
// 失败一律 401 且文案相同：区分「租户不存在 / 用户不存在 / 密码错」
// 就等于免费提供一个账号枚举接口。真实原因只进服务端日志。
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	if h.cfg.Auth == nil {
		fail(w, http.StatusNotImplemented, "认证未启用（兼容模式直接使用即可）")
		return
	}
	var body struct {
		Tenant   string `json:"tenant"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	slug := normalizeSlug(body.Tenant)
	email := normalizeEmail(body.Email)
	if slug == "" || email == "" || body.Password == "" {
		fail(w, http.StatusBadRequest, "tenant / email / password 都必填")
		return
	}

	u, err := h.cfg.Auth.Login(r.Context(), slug, email, body.Password)
	if err != nil {
		h.log.Printf("登录失败 tenant=%s email=%s: %v", slug, email, err)
		fail(w, http.StatusUnauthorized, "租户、邮箱或密码不正确")
		return
	}

	// refresh token 只有具体类型 auth.Service 能签（接口保持最小，见
	// issueLLMToken 的同款断言）。实现是别的 Authenticator 时退化为只发 access。
	var (
		token, refreshToken       string
		expiresAt, refreshExpires time.Time
	)
	if svc, ok := h.cfg.Auth.(*auth.Service); ok {
		token, expiresAt, refreshToken, refreshExpires, err = svc.IssuePair(u)
	} else {
		token, expiresAt, err = h.cfg.Auth.Issue(u)
	}
	if err != nil {
		h.failErr(w, err)
		return
	}
	tenant, err := h.cfg.Admin.GetTenant(r.Context(), u.TenantID)
	if err != nil {
		h.failErr(w, err)
		return
	}

	// 登录请求此时还没有 Principal（公开路由），注入一份让审计记对人和租户。
	// 刻意不在这里更新 LastLoginAt：Issue/Login 拿到的是完整 User，
	// 若实现返回的 User 不带 PasswordHash，UpdateUser 会把密码写空——
	// 登录统计不值得冒这个险。
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
		UserID:   u.ID,
		TenantID: u.TenantID,
		Email:    u.Email,
		Role:     u.Role,
		Via:      auth.ViaJWT,
	}))
	h.audit(r, "auth.login", "user", u.ID, map[string]string{"tenant": tenant.Slug})

	resp := map[string]any{
		"token":     token,
		"expiresAt": expiresAt,
		"user":      u,
		"tenant":    tenant,
	}
	// refreshToken 为空表示当前认证实现不支持刷新（见上面的类型断言）。
	// 字段仍带出来，前端可以用「有没有这个字段」判断要不要启用静默续期。
	if refreshToken != "" {
		resp["refreshToken"] = refreshToken
		resp["refreshExpiresAt"] = refreshExpires
	}
	writeJSON(w, http.StatusOK, resp)
}

// refresh 用 refresh token 换一对新 token（滚动续期）。
//
// 公开路由：refresh token 本身就是凭据。失败文案统一且不区分原因
// （过期 / 冒充 / 账号失效），原因只进服务端日志——这条路径上区分
// 「token 过期」和「账号被停用」同样是给账号枚举者递信息。
func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	svc, ok := h.cfg.Auth.(*auth.Service)
	if !ok {
		fail(w, http.StatusNotImplemented, "认证未启用 refresh 能力")
		return
	}
	var body struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.RefreshToken) == "" {
		fail(w, http.StatusBadRequest, "refreshToken 必填")
		return
	}

	u, err := svc.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		h.log.Printf("刷新失败: %v", err)
		fail(w, http.StatusUnauthorized, "刷新凭据无效或已过期，请重新登录")
		return
	}
	token, expiresAt, refreshToken, refreshExpires, err := svc.IssuePair(u)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":            token,
		"expiresAt":        expiresAt,
		"refreshToken":     refreshToken,
		"refreshExpiresAt": refreshExpires,
	})
}

// logout 只记审计，不维护黑名单。
//
// 权衡：JWT 是无状态的，要让一个 token 立刻失效就得引入吊销表（每请求一次查库）
// 或缩短有效期 + 刷新令牌（前端复杂度翻倍）。当前 24h 有效期下，
// 「点退出后 token 仍可用到过期」的代价小于上面两种方案，故选后者。
// 真需要即时吊销时（改密码、封号）应走 users.token_version 之类的字段。
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	p := h.principal(r)
	h.audit(r, "auth.logout", "user", p.UserID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// me 回当前身份。前端刷新页面第一件事就是调它，用来恢复顶栏和按钮权限。
func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	out := map[string]any{"principal": p}
	// 兼容模式（ViaDev）没有真实用户，只有 Principal，不硬造一个空 User。
	if p.UserID != "" {
		u, err := h.cfg.Admin.GetUser(r.Context(), p.UserID)
		if err != nil {
			h.failErr(w, err)
			return
		}
		out["user"] = u
	}
	writeJSON(w, http.StatusOK, out)
}

// changePassword 只改自己的密码。
//
// 接口上没有 userId 参数，这是刻意的：改别人的密码属于「重置」，
// 那条路在 admin_handlers.go 里，有独立的权限检查与审计。
// 把两者合成一个接口，迟早会有人只校验了登录态就把 userId 传进来。
func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	if p.UserID == "" {
		fail(w, http.StatusBadRequest, "当前为兼容模式，没有可修改的用户身份")
		return
	}
	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len([]rune(body.NewPassword)) < auth.MinPasswordLen {
		fail(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}

	u, err := h.cfg.Admin.GetUser(r.Context(), p.UserID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	// 必须先验旧密码：否则拿到一个没过期的 token 就能直接改掉密码，
	// 等于把「偷到 token」升级成「永久接管账号」。
	if err := auth.CheckPassword(u.PasswordHash, body.OldPassword); err != nil {
		fail(w, http.StatusUnauthorized, "原密码不正确")
		return
	}
	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u.PasswordHash = hash
	// 整体失效旧凭据：改密码之前签发的 access / refresh（包括别的设备上的
	// 登录态）在 iat 检查处全部作废。这是「不维护黑名单」的吊销点。
	u.TokenValidAfter = time.Now().UTC()
	if err := h.cfg.Admin.UpdateUser(r.Context(), u); err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "auth.password.change", "user", u.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ── 校验 ─────────────────────────────────────────────────────

// 密码、凭据、租户 slug 的正则不用 regexp：这几条规则都很短，
// 手写判断比一行正则更容易看出「边界在哪」。

// normalizeSlug 统一登录与建租户两侧的 slug 形态。
// 不做这层归一化的话，用户输 "Acme" 会因为库里存的是 "acme" 而登录失败，
// 而这时错误提示偏偏是「密码不正确」——最难查的那类问题。
func normalizeSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validSlug 限制租户短名：小写字母、数字、连字符，3~32 位。
// 不允许下划线与点号——它们会出现在前端路由和子域名里，两类字符在 URL 中含义不稳定。
func validSlug(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// validEmail 用 net/mail 解析再补两道检查。
//
// 为什么要额外要求域名带点：ParseAddress 会接受 a@localhost，
// 这在邮箱找回/通知场景下一定是笔误，早报错比事后收不到邮件好。
func validEmail(s string) bool {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>,;") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
