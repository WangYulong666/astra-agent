package auth

// 认证层的测试。
//
// 用内存假 store 而不是真库：这里要验的是「判定顺序与安全属性」，
// 搭一套 SQLite 只会让失败原因变模糊，还慢一个数量级。
// 假 store 只实现被调用的方法，其余故意留空实现——将来 Service 多调一个方法，
// 测试会立刻因为拿到零值而炸，不会静默漏测。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 假 store ──

type fakeStore struct {
	mu      sync.Mutex
	tenants map[string]store.Tenant
	users   map[string]store.User
	keys    map[string]store.APIKey // 按 key hash 索引，和真实实现一致

	// touched 用来在没有 sleep 的前提下确认异步记账确实发生了。
	touched chan string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tenants: map[string]store.Tenant{},
		users:   map[string]store.User{},
		keys:    map[string]store.APIKey{},
		touched: make(chan string, 4),
	}
}

func (f *fakeStore) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tenants[id]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tenants {
		if t.Slug == slug {
			return t, nil
		}
	}
	return store.Tenant{}, store.ErrNotFound
}

func (f *fakeStore) GetUser(_ context.Context, id string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) GetUserByEmail(_ context.Context, tenantID, email string) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.TenantID == tenantID && u.Email == email {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeStore) UpdateUser(_ context.Context, u store.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[u.ID]; !ok {
		return store.ErrNotFound
	}
	f.users[u.ID] = u
	return nil
}

func (f *fakeStore) GetAPIKeyByHash(_ context.Context, hash string) (store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[hash]
	if !ok {
		return store.APIKey{}, store.ErrNotFound
	}
	return k, nil
}

func (f *fakeStore) TouchAPIKey(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	for h, k := range f.keys {
		if k.ID == id {
			k.LastUsedAt = at
			f.keys[h] = k
			break
		}
	}
	f.mu.Unlock()
	select {
	case f.touched <- id:
	default: // 缓冲满了就丢，测试只关心「至少记过一次」
	}
	return nil
}

// 以下方法 Service 不用，返回零值即可。

func (f *fakeStore) CreateTenant(context.Context, store.Tenant) error { return nil }
func (f *fakeStore) ListTenants(context.Context) ([]store.Tenant, error) {
	return nil, nil
}
func (f *fakeStore) UpdateTenant(context.Context, store.Tenant) error { return nil }
func (f *fakeStore) CreateUser(context.Context, store.User) error     { return nil }
func (f *fakeStore) ListUsers(context.Context, string) ([]store.User, error) {
	return nil, nil
}
func (f *fakeStore) CountUsers(context.Context, string) (int, error) { return 0, nil }
func (f *fakeStore) CreateAPIKey(context.Context, store.APIKey) error {
	return nil
}
func (f *fakeStore) ListAPIKeys(context.Context, string) ([]store.APIKey, error) {
	return nil, nil
}
func (f *fakeStore) RevokeAPIKey(context.Context, string, string) error { return nil }
func (f *fakeStore) InsertUsage(context.Context, store.Usage) error     { return nil }
func (f *fakeStore) ListUsage(context.Context, store.UsageFilter) ([]store.Usage, error) {
	return nil, nil
}
func (f *fakeStore) SumUsage(context.Context, store.UsageFilter) (store.UsageSum, error) {
	return store.UsageSum{}, nil
}
func (f *fakeStore) InsertAudit(context.Context, store.AuditLog) error { return nil }
func (f *fakeStore) ListAudit(context.Context, store.AuditFilter) ([]store.AuditLog, error) {
	return nil, nil
}
func (f *fakeStore) ListSessionsByTenant(context.Context, string) ([]store.Session, error) {
	return nil, nil
}

// ── 测试夹具 ──

// testClock 让有效期逻辑不用 sleep 也能测。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// 测试用固定哈希，cost=4：本层验的是判定顺序，不验 bcrypt 强度（那由 password.go 的
// 常量保证）。真用 cost=12 的话，-race 下一次比较就要两秒，整个包会跑到分钟级。
const testPasswordHash = "$2a$04$hAa/CyDeV6wzkYU8BGeOzu020ab.hUrvCWlvd21MTx/hqARXRIQyO"

const (
	testPassword   = "correct-horse-battery"
	testEmail      = "alice@example.com"
	testTenantSlug = "acme"
)

var testSecret = []byte("test-secret-0123456789-abcdefghij")

type fixture struct {
	svc    *Service
	store  *fakeStore
	clock  *testClock
	tenant store.Tenant
	user   store.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		store: newFakeStore(),
		// 整秒开始，避开 JWT 的秒级截断带来的误差。
		clock: &testClock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)},
		tenant: store.Tenant{
			ID: "tnt_1", Name: "Acme", Slug: testTenantSlug, Status: store.StatusActive,
		},
		user: store.User{
			ID: "usr_1", TenantID: "tnt_1", Email: testEmail,
			DisplayName: "Alice", PasswordHash: testPasswordHash,
			Role: store.RoleOwner, Status: store.StatusActive,
		},
	}
	f.store.tenants[f.tenant.ID] = f.tenant
	f.store.users[f.user.ID] = f.user

	svc, err := New(Config{Store: f.store, Secret: testSecret, Now: f.clock.Now})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	f.svc = svc
	return f
}

// addKey 把一把凭据塞进假 store，返回明文。
func (f *fixture) addKey(t *testing.T, mutate func(*store.APIKey)) string {
	t.Helper()
	plain, _, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("生成 API Key 失败: %v", err)
	}
	k := store.APIKey{
		ID: "key_1", TenantID: f.tenant.ID, UserID: f.user.ID,
		Name: "cli", KeyHash: hash, CreatedAt: f.clock.Now(),
	}
	if mutate != nil {
		mutate(&k)
	}
	f.store.mu.Lock()
	f.store.keys[k.KeyHash] = k
	f.store.mu.Unlock()
	return plain
}

// ── New ──

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Secret: testSecret}); err == nil {
		t.Fatal("Store 为空时应报错")
	}
	// 短密钥等于可离线暴力猜解，必须在启动时就拦住。
	if _, err := New(Config{Store: newFakeStore(), Secret: []byte("short")}); err == nil {
		t.Fatal("密钥过短时应报错")
	}
	// 零值 TTL 走默认 24h。
	svc, err := New(Config{Store: newFakeStore(), Secret: testSecret})
	if err != nil {
		t.Fatalf("合法配置不应失败: %v", err)
	}
	if svc.ttl != DefaultTokenTTL {
		t.Fatalf("默认 TTL 应为 %v，实际 %v", DefaultTokenTTL, svc.ttl)
	}
}

// ── Login ──

func TestLogin(t *testing.T) {
	cases := []struct {
		name     string
		slug     string
		email    string
		password string
		mutate   func(f *fixture)
		wantErr  error
	}{
		{
			name: "成功", slug: testTenantSlug, email: testEmail, password: testPassword,
		},
		{
			name: "密码错误", slug: testTenantSlug, email: testEmail, password: "wrong-password",
			wantErr: ErrInvalidCredential,
		},
		{
			name: "用户不存在", slug: testTenantSlug, email: "nobody@example.com", password: testPassword,
			wantErr: ErrInvalidCredential,
		},
		{
			name: "租户不存在", slug: "ghost", email: testEmail, password: testPassword,
			wantErr: ErrInvalidCredential,
		},
		{
			name: "租户停用", slug: testTenantSlug, email: testEmail, password: testPassword,
			mutate: func(f *fixture) {
				t := f.tenant
				t.Status = store.StatusSuspended
				f.store.tenants[t.ID] = t
			},
			wantErr: ErrTenantSuspended,
		},
		{
			name: "用户停用", slug: testTenantSlug, email: testEmail, password: testPassword,
			mutate: func(f *fixture) {
				u := f.user
				u.Status = store.StatusSuspended
				f.store.users[u.ID] = u
			},
			wantErr: ErrInvalidCredential,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}

			u, err := f.svc.Login(context.Background(), tc.slug, tc.email, tc.password)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("期望错误 %v，实际 %v", tc.wantErr, err)
				}
				// 失败时不能顺手把用户带出去，否则 handler 里一个疏忽就泄漏了用户对象。
				if u.ID != "" {
					t.Fatalf("登录失败仍返回了用户 %q", u.ID)
				}
				return
			}
			if err != nil {
				t.Fatalf("登录应成功，实际 %v", err)
			}
			if u.ID != f.user.ID {
				t.Fatalf("返回了错误的用户: %q", u.ID)
			}
			if !u.LastLoginAt.Equal(f.clock.Now()) {
				t.Fatalf("last_login_at 应被更新为 %v，实际 %v", f.clock.Now(), u.LastLoginAt)
			}
		})
	}
}

// TestLoginEnumerationSameError 确认「用户不存在」与「密码错误」对外不可区分。
func TestLoginEnumerationSameError(t *testing.T) {
	f := newFixture(t)

	_, errNoUser := f.svc.Login(context.Background(), testTenantSlug, "nobody@example.com", testPassword)
	_, errBadPW := f.svc.Login(context.Background(), testTenantSlug, testEmail, "wrong-password")

	if !errors.Is(errNoUser, ErrInvalidCredential) || !errors.Is(errBadPW, ErrInvalidCredential) {
		t.Fatalf("两种失败都应是 ErrInvalidCredential，实际 %v / %v", errNoUser, errBadPW)
	}
	if errNoUser.Error() != errBadPW.Error() {
		t.Fatalf("错误文本不应有差异: %q vs %q", errNoUser, errBadPW)
	}
}

// ── JWT ──

func TestIssueAndAuthenticateJWT(t *testing.T) {
	f := newFixture(t)

	tok, exp, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if want := f.clock.Now().Add(DefaultTokenTTL); !exp.Equal(want) {
		t.Fatalf("过期时间应为 %v，实际 %v", want, exp)
	}

	p, err := f.svc.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if p.UserID != f.user.ID || p.TenantID != f.user.TenantID {
		t.Fatalf("身份不对: %+v", p)
	}
	if p.Via != ViaJWT {
		t.Fatalf("Via 应为 %s，实际 %s", ViaJWT, p.Via)
	}
	if p.KeyID != "" {
		t.Fatalf("JWT 身份不该有 KeyID: %q", p.KeyID)
	}
	if p.Role != store.RoleOwner {
		t.Fatalf("角色不对: %q", p.Role)
	}
}

// TestJWTRoleComesFromStore 降权之后旧 token 不该继续带着老权限。
func TestJWTRoleComesFromStore(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	u := f.user
	u.Role = store.RoleMember
	f.store.users[u.ID] = u

	p, err := f.svc.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if p.Role != store.RoleMember {
		t.Fatalf("角色应以库为准（member），实际 %q", p.Role)
	}
	if p.Can(ActionUserManage) {
		t.Fatal("降为 member 后不应还能管用户")
	}
}

func TestJWTExpired(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	f.clock.Advance(DefaultTokenTTL + time.Minute)
	if _, err := f.svc.Authenticate(context.Background(), tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期 token 应返回 ErrExpired，实际 %v", err)
	}
}

func TestJWTTamperedPayload(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 把 role 改成 owner 再原样拼回去：签名对不上，必须拒绝。
	// 这不是「格式错」，而是真正考验验签的用例。
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token 分段数异常: %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	claims["role"] = store.RoleOwner
	claims["tid"] = "tnt_evil"
	patched, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("payload 编码失败: %v", err)
	}
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(patched) + "." + parts[2]

	if _, err := f.svc.Authenticate(context.Background(), forged); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("篡改 payload 应返回 ErrInvalidCredential，实际 %v", err)
	}
}

func TestJWTRejectsAlgorithmConfusion(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	parts := strings.Split(tok, ".")

	// alg=none：最经典的绕过，把算法改成 none、签名留空。
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	noneToken := noneHeader + "." + parts[1] + "."
	if _, err := f.svc.Authenticate(context.Background(), noneToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("alg=none 应被拒绝，实际 %v", err)
	}

	// RS256 混淆：header 声称 RS256，签名仍是 HMAC。
	// 若实现把密钥交给 RSA 验签实现，公钥就会被当成 HMAC 密钥用。
	rsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	rsToken := rsHeader + "." + parts[1] + "." + parts[2]
	if _, err := f.svc.Authenticate(context.Background(), rsToken); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("RS256 混淆应被拒绝，实际 %v", err)
	}
}

// TestAuthenticateJWTStatusChecks 签名有效但账号/租户状态变了，也必须当场拦住。
func TestAuthenticateJWTStatusChecks(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *fixture)
		wantErr error
	}{
		{
			name: "租户被停用",
			mutate: func(f *fixture) {
				x := f.tenant
				x.Status = store.StatusSuspended
				f.store.tenants[x.ID] = x
			},
			wantErr: ErrTenantSuspended,
		},
		{
			name: "用户被停用",
			mutate: func(f *fixture) {
				x := f.user
				x.Status = store.StatusSuspended
				f.store.users[x.ID] = x
			},
			wantErr: ErrInvalidCredential,
		},
		{
			name:    "用户被删除",
			mutate:  func(f *fixture) { delete(f.store.users, f.user.ID) },
			wantErr: ErrInvalidCredential,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tok, _, err := f.svc.Issue(f.user)
			if err != nil {
				t.Fatalf("签发失败: %v", err)
			}
			tc.mutate(f)
			if _, err := f.svc.Authenticate(context.Background(), tok); !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestAuthenticateEmptyToken(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Authenticate(context.Background(), "   "); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("空凭据应返回 ErrNoCredential，实际 %v", err)
	}
}

// ── API Key ──

func TestAPIKeyAuthenticate(t *testing.T) {
	f := newFixture(t)
	plain := f.addKey(t, nil)

	p, err := f.svc.Authenticate(context.Background(), plain)
	if err != nil {
		t.Fatalf("API Key 认证失败: %v", err)
	}
	if p.Via != ViaAPIKey || p.KeyID != "key_1" {
		t.Fatalf("身份不对: %+v", p)
	}
	if p.TenantID != f.tenant.ID || p.UserID != f.user.ID {
		t.Fatalf("身份不对: %+v", p)
	}

	// 异步记账：等它落地，确认没被忘掉（也不 sleep）。
	select {
	case id := <-f.store.touched:
		if id != "key_1" {
			t.Fatalf("记账的 key 不对: %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("last_used_at 未被异步更新")
	}
}

func TestAPIKeyRevoked(t *testing.T) {
	f := newFixture(t)
	plain := f.addKey(t, func(k *store.APIKey) {
		k.RevokedAt = f.clock.Now().Add(-time.Minute)
	})

	if _, err := f.svc.Authenticate(context.Background(), plain); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("已撤销的 key 应返回 ErrInvalidCredential，实际 %v", err)
	}
}

func TestAPIKeyExpired(t *testing.T) {
	f := newFixture(t)
	plain := f.addKey(t, func(k *store.APIKey) {
		k.ExpiresAt = f.clock.Now().Add(-time.Second)
	})

	if _, err := f.svc.Authenticate(context.Background(), plain); !errors.Is(err, ErrExpired) {
		t.Fatalf("已过期的 key 应返回 ErrExpired，实际 %v", err)
	}
}

func TestAPIKeyUnknown(t *testing.T) {
	f := newFixture(t)
	// 形状合法但库里没有：不能因为「长得像我们的 key」就放行。
	if _, err := f.svc.Authenticate(context.Background(), APIKeyPrefix+"notarealkey"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("未知 key 应返回 ErrInvalidCredential，实际 %v", err)
	}
}

// TestAPIKeyOwnerSuspended key 有效但持有者被停用，必须拒绝。
func TestAPIKeyOwnerSuspended(t *testing.T) {
	f := newFixture(t)
	plain := f.addKey(t, nil)
	u := f.user
	u.Status = store.StatusSuspended
	f.store.users[u.ID] = u

	if _, err := f.svc.Authenticate(context.Background(), plain); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("持有者停用应返回 ErrInvalidCredential，实际 %v", err)
	}
}

// ── 虚拟 key ──

func TestVirtualKeyRoundTrip(t *testing.T) {
	f := newFixture(t)

	tok, exp, err := f.svc.IssueVirtualKey(f.tenant.ID, f.user.ID, "ses_1", 0) // 0 → 默认 TTL
	if err != nil {
		t.Fatalf("签发虚拟 key 失败: %v", err)
	}
	if want := f.clock.Now().Add(DefaultVirtualKeyTTL); !exp.Equal(want) {
		t.Fatalf("默认过期时间应为 %v，实际 %v", want, exp)
	}

	c, err := f.svc.VerifyVirtualKey(tok)
	if err != nil {
		t.Fatalf("校验虚拟 key 失败: %v", err)
	}
	if c.UserID != f.user.ID || c.TenantID != f.tenant.ID || c.SessionID != "ses_1" {
		t.Fatalf("claims 不对: %+v", c)
	}
	if c.Scope != VirtualKeyScope {
		t.Fatalf("scope 应为 %q，实际 %q", VirtualKeyScope, c.Scope)
	}

	f.clock.Advance(DefaultVirtualKeyTTL + time.Minute)
	if _, err := f.svc.VerifyVirtualKey(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期虚拟 key 应返回 ErrExpired，实际 %v", err)
	}
}

// TestVirtualKeySeparatedFromJWT 两种 token 共用一个密钥，只能靠 scope 分开。
// 这条一旦失效，就等于把网关凭据升级成了平台全权身份。
func TestVirtualKeySeparatedFromJWT(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	vk, _, err := f.svc.IssueVirtualKey(f.tenant.ID, f.user.ID, "ses_1", time.Hour)
	if err != nil {
		t.Fatalf("签发虚拟 key 失败: %v", err)
	}
	jwtTok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发 JWT 失败: %v", err)
	}

	t.Run("虚拟 key 不能当平台 token", func(t *testing.T) {
		if _, err := f.svc.Authenticate(ctx, vk); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("应拒绝，实际 %v", err)
		}
	})

	t.Run("平台 token 不能当虚拟 key", func(t *testing.T) {
		if _, err := f.svc.VerifyVirtualKey(jwtTok); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("应拒绝，实际 %v", err)
		}
	})
}

func TestIssueVirtualKeyRequiresIDs(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.IssueVirtualKey("", f.user.ID, "ses_1", time.Hour); err == nil {
		t.Fatal("缺租户时应报错")
	}
	if _, _, err := f.svc.IssueVirtualKey(f.tenant.ID, f.user.ID, "", time.Hour); err == nil {
		t.Fatal("缺会话时应报错")
	}
}
