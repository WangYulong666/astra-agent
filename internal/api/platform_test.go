package api

// 平台层单测：内存版 AdminStore + Authenticator，除 httptest 外不起任何真实依赖。
//
// 为什么不复用 api_test.go 的 harness：那个 harness 会现编 acp-proxy 与 mock agent，
// 而治理层的错几乎都发生在「归属判断」上，跟沙箱进程无关。用假实现能把
// 「跨租户」「member 越权」直接摆出来，也跑得快。
// api_test.go 不动（它验的是运行时链路），脚手架因此单独放这里。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 假存储 ───────────────────────────────────────────────────

type fakeAdmin struct {
	mu       sync.Mutex
	tenants  []store.Tenant
	users    []store.User
	keys     []store.APIKey
	usage    []store.Usage
	audits   []store.AuditLog
	sessions []store.Session
}

// ── 租户 ──

func (f *fakeAdmin) CreateTenant(_ context.Context, t store.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.tenants {
		if x.Slug == t.Slug || x.ID == t.ID {
			return store.ErrDuplicate
		}
	}
	f.tenants = append(f.tenants, t)
	return nil
}

func (f *fakeAdmin) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tenants {
		if t.ID == id {
			return t, nil
		}
	}
	return store.Tenant{}, store.ErrNotFound
}

func (f *fakeAdmin) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tenants {
		if t.Slug == slug {
			return t, nil
		}
	}
	return store.Tenant{}, store.ErrNotFound
}

func (f *fakeAdmin) ListTenants(context.Context) ([]store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Tenant(nil), f.tenants...), nil
}

func (f *fakeAdmin) UpdateTenant(_ context.Context, t store.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.tenants {
		if x.ID == t.ID {
			f.tenants[i] = t
			return nil
		}
	}
	return store.ErrNotFound
}

// ── 用户 ──

func (f *fakeAdmin) CreateUser(_ context.Context, u store.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.users {
		if x.TenantID == u.TenantID && strings.EqualFold(x.Email, u.Email) {
			return store.ErrDuplicate
		}
	}
	f.users = append(f.users, u)
	return nil
}

func (f *fakeAdmin) GetUser(_ context.Context, id string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeAdmin) GetUserByEmail(_ context.Context, tenantID, email string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.TenantID == tenantID && strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeAdmin) ListUsers(_ context.Context, tenantID string) ([]store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.User
	for _, u := range f.users {
		if tenantID == "" || u.TenantID == tenantID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeAdmin) UpdateUser(_ context.Context, u store.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.users {
		if x.ID == u.ID && x.TenantID == u.TenantID {
			f.users[i] = u
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeAdmin) CountUsers(_ context.Context, tenantID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.users {
		if u.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

// ── 凭据 ──

func (f *fakeAdmin) CreateAPIKey(_ context.Context, k store.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakeAdmin) GetAPIKeyByHash(_ context.Context, hash string) (store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.KeyHash == hash {
			return k, nil
		}
	}
	return store.APIKey{}, store.ErrNotFound
}

func (f *fakeAdmin) ListAPIKeys(_ context.Context, tenantID string) ([]store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.APIKey
	for _, k := range f.keys {
		if tenantID == "" || k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeAdmin) RevokeAPIKey(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, k := range f.keys {
		if k.ID == id && (tenantID == "" || k.TenantID == tenantID) {
			f.keys[i].RevokedAt = time.Now().UTC()
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeAdmin) TouchAPIKey(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, k := range f.keys {
		if k.ID == id {
			f.keys[i].LastUsedAt = at
			return nil
		}
	}
	return store.ErrNotFound
}

// ── 用量 ──

func matchUsage(u store.Usage, fl store.UsageFilter) bool {
	if fl.TenantID != "" && u.TenantID != fl.TenantID {
		return false
	}
	if fl.UserID != "" && u.UserID != fl.UserID {
		return false
	}
	if fl.SessionID != "" && u.SessionID != fl.SessionID {
		return false
	}
	if !fl.From.IsZero() && u.CreatedAt.Before(fl.From) {
		return false
	}
	if !fl.To.IsZero() && u.CreatedAt.After(fl.To) {
		return false
	}
	return true
}

func (f *fakeAdmin) InsertUsage(_ context.Context, u store.Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, u)
	return nil
}

func (f *fakeAdmin) ListUsage(_ context.Context, fl store.UsageFilter) ([]store.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Usage
	for _, u := range f.usage {
		if matchUsage(u, fl) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if fl.Limit > 0 && len(out) > fl.Limit {
		out = out[:fl.Limit]
	}
	return out, nil
}

func (f *fakeAdmin) SumUsage(_ context.Context, fl store.UsageFilter) (store.UsageSum, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s store.UsageSum
	for _, u := range f.usage {
		if !matchUsage(u, fl) {
			continue
		}
		s.InputTokens += u.InputTokens
		s.OutputTokens += u.OutputTokens
		s.CostMicroUSD += u.CostMicroUSD
		s.Calls++
	}
	return s, nil
}

// ── 审计 ──

func (f *fakeAdmin) InsertAudit(_ context.Context, a store.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, a)
	return nil
}

func (f *fakeAdmin) ListAudit(_ context.Context, fl store.AuditFilter) ([]store.AuditLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AuditLog
	for _, a := range f.audits {
		if fl.TenantID != "" && a.TenantID != fl.TenantID {
			continue
		}
		if fl.Action != "" && a.Action != fl.Action {
			continue
		}
		if !fl.From.IsZero() && a.CreatedAt.Before(fl.From) {
			continue
		}
		if !fl.To.IsZero() && a.CreatedAt.After(fl.To) {
			continue
		}
		out = append(out, a)
	}
	if fl.Limit > 0 && len(out) > fl.Limit {
		out = out[:fl.Limit]
	}
	return out, nil
}

func (f *fakeAdmin) ListSessionsByTenant(_ context.Context, tenantID string) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, s := range f.sessions {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	return out, nil
}

// ── 假认证器 ─────────────────────────────────────────────────

// fakeAuth 只复刻最小语义：按 slug 找租户、按 email 找用户、比对 bcrypt。
// token 用 "tok-<userID>"，省掉签名——这里测的是 handler 的归属判断，不是 JWT 本身。
type fakeAuth struct{ db *fakeAdmin }

const testPassword = "pw-123456"

func (a *fakeAuth) Login(ctx context.Context, slug, email, password string) (store.User, error) {
	t, err := a.db.GetTenantBySlug(ctx, slug)
	if err != nil {
		return store.User{}, auth.ErrInvalidCredential
	}
	u, err := a.db.GetUserByEmail(ctx, t.ID, email)
	if err != nil {
		return store.User{}, auth.ErrInvalidCredential
	}
	if u.Status != store.StatusActive {
		return store.User{}, auth.ErrInvalidCredential
	}
	if err := auth.CheckPassword(u.PasswordHash, password); err != nil {
		return store.User{}, auth.ErrInvalidCredential
	}
	return u, nil
}

func (a *fakeAuth) Issue(u store.User) (string, time.Time, error) {
	return "tok-" + u.ID, time.Now().Add(24 * time.Hour), nil
}

func (a *fakeAuth) Authenticate(ctx context.Context, token string) (auth.Principal, error) {
	id := strings.TrimPrefix(token, "tok-")
	if id == token || id == "" {
		return auth.Principal{}, auth.ErrInvalidCredential
	}
	u, err := a.db.GetUser(ctx, id)
	if err != nil {
		return auth.Principal{}, auth.ErrInvalidCredential
	}
	if u.Status != store.StatusActive {
		return auth.Principal{}, auth.ErrInvalidCredential
	}
	return auth.Principal{UserID: u.ID, TenantID: u.TenantID, Email: u.Email, Role: u.Role, Via: auth.ViaJWT}, nil
}

// ── 脚手架 ───────────────────────────────────────────────────

type platformHarness struct {
	srv *httptest.Server
	db  *fakeAdmin
}

func newPlatformHarness(t *testing.T) *platformHarness {
	t.Helper()
	db := &fakeAdmin{}
	h := &handler{
		cfg: Config{Auth: &fakeAuth{db: db}, Admin: db, RequireAuth: true},
		log: log.New(io.Discard, "", 0),
	}
	// 复用真实路由注册（含权限标注），这样「哪个接口要什么权限」也一起被覆盖。
	mux := http.NewServeMux()
	h.registerPlatform(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &platformHarness{srv: srv, db: db}
}

func (p *platformHarness) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, p.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// seedTenant 造最小世界：一个租户 + owner/admin/member 三个用户，密码都是 testPassword。
// 返回值是 access token（直接用 "tok-<id>"，绕过签名）。
func (p *platformHarness) seedTenant(t *testing.T) (tenantID, ownerTok, adminTok, memberTok string) {
	t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tenantID = "t1"
	p.db.tenants = append(p.db.tenants, store.Tenant{
		ID: tenantID, Name: "Acme", Slug: "acme", Status: store.StatusActive,
		MonthlyTokenLimit: 1000, CreatedAt: now,
	})
	mk := func(id, email, role string) string {
		p.db.users = append(p.db.users, store.User{
			ID: id, TenantID: tenantID, Email: email, DisplayName: email,
			PasswordHash: hash, Role: role, Status: store.StatusActive, CreatedAt: now,
		})
		return "tok-" + id
	}
	return tenantID,
		mk("u-owner", "owner@acme.test", store.RoleOwner),
		mk("u-admin", "admin@acme.test", store.RoleAdmin),
		mk("u-member", "member@acme.test", store.RoleMember)
}

// ── 用例 ─────────────────────────────────────────────────────

// TestBootstrapOnlyOnce 是这一组里最重要的：接口不需要凭据，
// 如果第二次还能成功，它就是一个「任何人都能注册」的洞。
func TestBootstrapOnlyOnce(t *testing.T) {
	p := newPlatformHarness(t)
	body := map[string]any{
		"tenantName": "Acme",
		"slug":       "acme",
		"email":      "owner@acme.test",
		"password":   testPassword,
	}
	code, out := p.do(t, http.MethodPost, "/v1/auth/bootstrap", "", body)
	if code != http.StatusCreated {
		t.Fatalf("首次 bootstrap 返回 %d: %s", code, out)
	}
	var got struct {
		Tenant store.Tenant `json:"tenant"`
		User   store.User   `json:"user"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Tenant.ID == "" || got.User.ID == "" {
		t.Fatalf("响应缺少 tenant/user: %s", out)
	}
	if got.User.Role != store.RoleOwner || got.User.TenantID != got.Tenant.ID {
		t.Fatalf("owner 归属不对: %+v", got.User)
	}
	if strings.Contains(string(out), "$2a$") || strings.Contains(string(out), "passwordHash") {
		t.Fatalf("响应里出现了密码哈希: %s", out)
	}

	code, out = p.do(t, http.MethodPost, "/v1/auth/bootstrap", "", map[string]any{
		"tenantName": "Evil", "slug": "evil", "email": "x@evil.test", "password": testPassword,
	})
	if code != http.StatusConflict {
		t.Fatalf("重复 bootstrap 返回 %d，期望 409: %s", code, out)
	}
	if n := len(p.db.tenants); n != 1 {
		t.Fatalf("租户数 = %d，重复初始化必须被挡住", n)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	p := newPlatformHarness(t)
	p.seedTenant(t)

	code, out := p.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{
		"tenant": "acme", "email": "owner@acme.test", "password": "wrong-password",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("密码错误返回 %d: %s", code, out)
	}
	// 不区分原因：租户不存在必须是同一句话，否则等于送一个账号枚举接口。
	code2, out2 := p.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{
		"tenant": "nope", "email": "ghost@acme.test", "password": "wrong-password",
	})
	if code2 != http.StatusUnauthorized {
		t.Fatalf("不存在的租户返回 %d: %s", code2, out2)
	}
	if string(out) != string(out2) {
		t.Fatalf("两种失败的响应体不同，泄漏了账号是否存在:\n%s\n%s", out, out2)
	}

	code, out = p.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{
		"tenant": "acme", "email": "owner@acme.test", "password": testPassword,
	})
	if code != http.StatusOK {
		t.Fatalf("正确密码返回 %d: %s", code, out)
	}
	var resp struct {
		Token  string `json:"token"`
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.Tenant.ID != "t1" {
		t.Fatalf("登录响应不对: %s", out)
	}
}

// TestMemberUsageCannotReadOthers 同时覆盖两条独立防线：
// query 里的 userId 必须被 Principal.UserID 覆盖，tenantID 只能来自 Principal。
func TestMemberUsageCannotReadOthers(t *testing.T) {
	p := newPlatformHarness(t)
	tenantID, ownerTok, _, memberTok := p.seedTenant(t)
	now := time.Now().UTC()
	for _, x := range []struct {
		userID string
		tokens int64
	}{{"u-owner", 900}, {"u-member", 10}} {
		p.db.usage = append(p.db.usage, store.Usage{
			ID: "us-" + x.userID, TenantID: tenantID, UserID: x.userID,
			Provider: "deepseek", Model: "chat", InputTokens: x.tokens,
			CostMicroUSD: x.tokens * 1000, Source: "gateway", CreatedAt: now,
		})
	}
	// 别的租户的账，任何一次响应里都不该出现。
	p.db.usage = append(p.db.usage, store.Usage{
		ID: "us-other", TenantID: "t2", UserID: "u-other",
		Provider: "deepseek", Model: "chat", InputTokens: 100000,
		CostMicroUSD: 999999999, Source: "gateway", CreatedAt: now,
	})

	code, out := p.do(t, http.MethodGet, "/v1/usage?userId=u-owner", memberTok, nil)
	if code != http.StatusOK {
		t.Fatalf("member 查用量返回 %d: %s", code, out)
	}
	var rep usageReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 1 || rep.Items[0].UserID != "u-member" {
		t.Fatalf("member 看到了别人的用量: %+v", rep.Items)
	}
	if rep.Sum.InputTokens != 10 {
		t.Fatalf("汇总 = %d，应只剩自己的 10", rep.Sum.InputTokens)
	}

	code, out = p.do(t, http.MethodGet, "/v1/usage", ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner 查用量返回 %d: %s", code, out)
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 2 {
		t.Fatalf("owner 看到 %d 条，应为本租户 2 条（跨租户那条不算）", len(rep.Items))
	}

	code, out = p.do(t, http.MethodGet, "/v1/usage/billing", memberTok, nil)
	if code != http.StatusOK {
		t.Fatalf("billing 返回 %d: %s", code, out)
	}
	var bill struct {
		InputTokens int64   `json:"inputTokens"`
		CostUsd     float64 `json:"costUsd"`
		Limit       int64   `json:"monthlyTokenLimit"`
		UsedPercent float64 `json:"usedPercent"`
	}
	if err := json.Unmarshal(out, &bill); err != nil {
		t.Fatal(err)
	}
	if bill.InputTokens != 10 || bill.CostUsd != 0.01 || bill.Limit != 1000 || bill.UsedPercent != 1 {
		t.Fatalf("billing 数字不对: %s", out)
	}
}

// TestAPIKeyPlaintextOnlyOnce：明文只允许出现一次，
// 列表一旦开始回显，key 就和密码没区别了。
func TestAPIKeyPlaintextOnlyOnce(t *testing.T) {
	p := newPlatformHarness(t)
	_, _, adminTok, memberTok := p.seedTenant(t)

	code, out := p.do(t, http.MethodPost, "/v1/admin/apikeys", adminTok, map[string]any{"name": "ci"})
	if code != http.StatusCreated {
		t.Fatalf("签发凭据返回 %d: %s", code, out)
	}
	var created struct {
		Key       store.APIKey `json:"key"`
		Plaintext string       `json:"plaintext"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Plaintext, auth.APIKeyPrefix) {
		t.Fatalf("明文格式不对: %q", created.Plaintext)
	}
	if created.Key.Prefix == "" || created.Key.KeyHash != "" {
		t.Fatalf("创建响应缺 Prefix 或泄漏 KeyHash: %+v", created.Key)
	}
	// 库里必须是摘要，不是明文。
	if p.db.keys[0].KeyHash == "" || p.db.keys[0].KeyHash == created.Plaintext {
		t.Fatal("库存的不是 sha256 摘要")
	}

	code, out = p.do(t, http.MethodGet, "/v1/admin/apikeys", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("列表返回 %d: %s", code, out)
	}
	if strings.Contains(string(out), created.Plaintext) {
		t.Fatal("列表接口回显了明文凭据")
	}
	if strings.Contains(string(out), p.db.keys[0].KeyHash) {
		t.Fatal("列表接口回显了 KeyHash")
	}
	if !strings.Contains(string(out), created.Key.Prefix) {
		t.Fatal("列表里应有 Prefix，否则用户分不清是哪把钥匙")
	}

	// member 没有 key:manage，签名入口本身就该被 403 挡住。
	code, _ = p.do(t, http.MethodPost, "/v1/admin/apikeys", memberTok, map[string]any{"name": "x"})
	if code != http.StatusForbidden {
		t.Fatalf("member 签发凭据返回 %d，期望 403", code)
	}
}

// TestLastOwnerProtected：owner 是唯一能管租户的角色，
// 把他降级或停用之后租户就没人能管了，只能改库。
func TestLastOwnerProtected(t *testing.T) {
	p := newPlatformHarness(t)
	_, ownerTok, adminTok, _ := p.seedTenant(t)

	code, out := p.do(t, http.MethodPatch, "/v1/admin/users/u-owner", ownerTok, map[string]any{"role": store.RoleMember})
	if code != http.StatusConflict {
		t.Fatalf("降级最后一个 owner 返回 %d，期望 409: %s", code, out)
	}
	code, out = p.do(t, http.MethodPatch, "/v1/admin/users/u-owner", ownerTok, map[string]any{"status": store.StatusSuspended})
	if code != http.StatusConflict {
		t.Fatalf("停用最后一个 owner 返回 %d，期望 409: %s", code, out)
	}

	// 再补一个 owner 之后，降级第一个就应当被允许。
	code, out = p.do(t, http.MethodPost, "/v1/admin/users", ownerTok, map[string]any{
		"email": "second@acme.test", "role": store.RoleOwner,
	})
	if code != http.StatusCreated {
		t.Fatalf("创建第二个 owner 返回 %d: %s", code, out)
	}
	var created struct {
		User            store.User `json:"user"`
		InitialPassword string     `json:"initialPassword"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}
	if created.InitialPassword == "" {
		t.Fatal("密码为空时应生成初始密码并回一次")
	}
	if created.User.PasswordHash != "" {
		t.Fatal("响应里带了 PasswordHash")
	}

	code, out = p.do(t, http.MethodPatch, "/v1/admin/users/u-owner", ownerTok, map[string]any{"role": store.RoleMember})
	if code != http.StatusOK {
		t.Fatalf("有第二个 owner 后降级应成功，实际 %d: %s", code, out)
	}

	// admin 不能授予 owner（否则 admin 与 owner 就没区别了）。
	code, _ = p.do(t, http.MethodPatch, "/v1/admin/users/u-admin", adminTok, map[string]any{"role": store.RoleOwner})
	if code != http.StatusForbidden {
		t.Fatalf("admin 自我提权返回 %d，期望 403", code)
	}
}

// TestCrossTenantIs404：越权一律 404。403 会告诉对方「这个 ID 存在」。
func TestCrossTenantIs404(t *testing.T) {
	p := newPlatformHarness(t)
	_, ownerTok, adminTok, _ := p.seedTenant(t)
	// 另一个租户的用户与租户本体。
	p.db.tenants = append(p.db.tenants, store.Tenant{
		ID: "t2", Name: "Globex", Slug: "globex", Status: store.StatusActive, CreatedAt: time.Now().UTC(),
	})
	p.db.users = append(p.db.users, store.User{
		ID: "u-foreign", TenantID: "t2", Email: "boss@globex.test",
		Role: store.RoleOwner, Status: store.StatusActive,
	})

	code, out := p.do(t, http.MethodPatch, "/v1/admin/users/u-foreign", adminTok, map[string]any{"status": store.StatusSuspended})
	if code != http.StatusNotFound {
		t.Fatalf("跨租户改用户返回 %d，期望 404: %s", code, out)
	}
	// 读租户要 tenant:manage（owner 才有），换 owner 的 token 再验一次归属。
	code, out = p.do(t, http.MethodGet, "/v1/admin/tenants/t2", ownerTok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("跨租户读租户返回 %d，期望 404: %s", code, out)
	}
	// admin 没有 tenant:manage，这一层是 403（权限不足，与归属无关）。
	code, out = p.do(t, http.MethodGet, "/v1/admin/tenants/t2", adminTok, nil)
	if code != http.StatusForbidden {
		t.Fatalf("admin 读租户返回 %d，期望 403: %s", code, out)
	}
	// 审计只能看本租户的。
	p.db.audits = append(p.db.audits, store.AuditLog{ID: "a1", TenantID: "t2", Action: "secret.action", CreatedAt: time.Now().UTC()})
	code, out = p.do(t, http.MethodGet, "/v1/admin/audit", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("审计返回 %d: %s", code, out)
	}
	if strings.Contains(string(out), "secret.action") {
		t.Fatal("审计接口返回了别的租户的记录")
	}
}
