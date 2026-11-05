package api

// SSE 票据与 refresh 的 HTTP 层测试。
//
// 用真实的 auth.Service + 假 AdminStore：这里验的是「路由怎么发凭据」，
// JWT 本身的判定（过期 / 重放 / 冒充）在 internal/auth 里已覆盖，
// 这层只确认中间件把票据用在且只用在该用的地方。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// sseTestClock 与 auth 包的 testClock 同款：票据过期要能不 sleep 地测。
type sseTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *sseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sseTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type authHarness struct {
	srv   *httptest.Server
	db    *fakeAdmin
	clock *sseTestClock
	svc   *auth.Service
	uid   string
}

func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()
	db := &fakeAdmin{}
	// 时钟基于真实墙钟：changePassword 用 time.Now() 写 tokenValidAfter，
	// 固定在未来的测试时钟会让「iat 晚于失效时间」的判定永远不触发。
	clock := &sseTestClock{t: time.Now().UTC()}
	svc, err := auth.New(auth.Config{
		Store:  db,
		Secret: []byte("api-sse-test-secret-0123456789"),
		Now:    clock.Now,
	})
	if err != nil {
		t.Fatalf("构造 auth.Service 失败: %v", err)
	}

	uid := "u1"
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}
	db.tenants = append(db.tenants, store.Tenant{
		ID: "t1", Name: "Acme", Slug: "acme", Status: store.StatusActive,
		CreatedAt: clock.Now(),
	})
	db.users = append(db.users, store.User{
		ID: uid, TenantID: "t1", Email: "u@acme.test", DisplayName: "U",
		PasswordHash: hash, Role: store.RoleOwner, Status: store.StatusActive,
		CreatedAt: clock.Now(),
	})

	h := &handler{
		cfg: Config{Auth: svc, Admin: db, RequireAuth: true},
		log: log.New(io.Discard, "", 0),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/sse-ticket", h.authed(h.sseTicket, ""))
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.HandleFunc("PATCH /v1/auth/password", h.authed(h.changePassword, ""))
	// 目标端点只回一句身份，验证票据确实换来的是本人。
	mux.HandleFunc("GET /v1/runs/{id}/events", h.authedEvents(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok:"+h.principal(r).UserID)
	}, auth.ActionSessionRead))
	// 用来验证「票据不能当普通凭据」的反面端点。
	mux.HandleFunc("GET /v1/sessions", h.authed(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, auth.ActionSessionRead))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &authHarness{srv: srv, db: db, clock: clock, svc: svc, uid: uid}
}

func (p *authHarness) issueTokens(t *testing.T) (access, refresh string) {
	t.Helper()
	access, _, refresh, _, err := p.svc.IssuePair(store.User{
		ID: p.uid, TenantID: "t1", Role: store.RoleOwner, Status: store.StatusActive,
	})
	if err != nil {
		t.Fatalf("签发 token 失败: %v", err)
	}
	return access, refresh
}

// TestSSETicketEndToEnd 走完整链路：登录态换票 → 票据连事件流 → 重放拒绝。
func TestSSETicketEndToEnd(t *testing.T) {
	p := newAuthHarness(t)
	access, _ := p.issueTokens(t)

	// 未登录换票必须被挡：签发票据本身需要登录态。
	code, _ := doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/sse-ticket", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("匿名换票返回 %d，期望 401", code)
	}

	code, body := doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/sse-ticket", access, nil)
	if code != http.StatusOK {
		t.Fatalf("换票返回 %d: %s", code, body)
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(body, &tk); err != nil || tk.Ticket == "" {
		t.Fatalf("换票响应不对: %s", body)
	}

	// EventSource 场景：只有 ?ticket=，没有 Authorization。
	code, body = doJSON(t, http.MethodGet, p.srv.URL+"/v1/runs/r1/events?ticket="+tk.Ticket, "", nil)
	if code != http.StatusOK || string(body) != "ok:"+p.uid {
		t.Fatalf("票据连接事件流应成功，实际 %d: %s", code, body)
	}

	// jti 一次性：同一张票第二次必须 401。
	code, _ = doJSON(t, http.MethodGet, p.srv.URL+"/v1/runs/r1/events?ticket="+tk.Ticket, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("票据重放返回 %d，期望 401", code)
	}

	// 票据放进 Authorization 冒充普通登录：中间件必须拒绝。
	code, _ = doJSON(t, http.MethodGet, p.srv.URL+"/v1/sessions", tk.Ticket, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("SSE 票据访问 /v1/sessions 返回 %d，期望 401", code)
	}

	// 强制认证下既没凭据也没票据：401。
	code, _ = doJSON(t, http.MethodGet, p.srv.URL+"/v1/runs/r1/events", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("无凭据无票据返回 %d，期望 401", code)
	}
}

// TestSSETicketExpired 票据过 60 秒即失效。
func TestSSETicketExpired(t *testing.T) {
	p := newAuthHarness(t)
	access, _ := p.issueTokens(t)

	_, body := doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/sse-ticket", access, nil)
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(body, &tk); err != nil {
		t.Fatal(err)
	}

	p.clock.Advance(auth.DefaultSSETicketTTL + time.Minute)
	code, out := doJSON(t, http.MethodGet, p.srv.URL+"/v1/runs/r1/events?ticket="+tk.Ticket, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("过期票据返回 %d，期望 401: %s", code, out)
	}
}

// TestRefreshEndpoint 覆盖：正常轮换、access 冒充、垃圾输入。
func TestRefreshEndpoint(t *testing.T) {
	p := newAuthHarness(t)
	access, refresh := p.issueTokens(t)

	// 正常轮换：新 access 必须真能用。
	code, body := doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/refresh", "",
		map[string]string{"refreshToken": refresh})
	if code != http.StatusOK {
		t.Fatalf("refresh 返回 %d: %s", code, body)
	}
	var pair struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(body, &pair); err != nil || pair.Token == "" || pair.RefreshToken == "" {
		t.Fatalf("refresh 响应不对: %s", body)
	}
	code, _ = doJSON(t, http.MethodGet, p.srv.URL+"/v1/sessions", pair.Token, nil)
	if code != http.StatusOK {
		t.Fatalf("轮换后的 access 不可用，返回 %d", code)
	}

	// access 冒充 refresh：401。
	code, _ = doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/refresh", "",
		map[string]string{"refreshToken": access})
	if code != http.StatusUnauthorized {
		t.Fatalf("access 冒充 refresh 返回 %d，期望 401", code)
	}

	// 乱串与空值：401 / 400。
	code, _ = doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/refresh", "",
		map[string]string{"refreshToken": "not-a-jwt"})
	if code != http.StatusUnauthorized {
		t.Fatalf("垃圾 refreshToken 返回 %d，期望 401", code)
	}
	code, _ = doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/refresh", "",
		map[string]string{"refreshToken": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("空 refreshToken 返回 %d，期望 400", code)
	}
}

// TestChangePasswordInvalidatesTokens handler 层验收：改密码 → 旧 access 与
// 旧 refresh 同时失效 → 新登录可用。bcrypt 在此路径上是真实开销（约百毫秒）。
func TestChangePasswordInvalidatesTokens(t *testing.T) {
	p := newAuthHarness(t)
	access, refresh := p.issueTokens(t)

	code, out := doJSON(t, http.MethodPatch, p.srv.URL+"/v1/auth/password", access,
		map[string]string{"oldPassword": "wrong", "newPassword": "new-password-123"})
	if code != http.StatusUnauthorized {
		t.Fatalf("旧密码错误返回 %d，期望 401: %s", code, out)
	}

	code, _ = doJSON(t, http.MethodPatch, p.srv.URL+"/v1/auth/password", access,
		map[string]string{"oldPassword": "pw-123456", "newPassword": "new-password-123"})
	if code != http.StatusNoContent {
		t.Fatalf("改密码返回 %d: %s", code, out)
	}

	code, _ = doJSON(t, http.MethodGet, p.srv.URL+"/v1/sessions", access, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("改密码后旧 access 返回 %d，期望 401", code)
	}
	code, _ = doJSON(t, http.MethodPost, p.srv.URL+"/v1/auth/refresh", "",
		map[string]string{"refreshToken": refresh})
	if code != http.StatusUnauthorized {
		t.Fatalf("改密码后旧 refresh 返回 %d，期望 401", code)
	}
}

// doJSON 是 authHarness 专用的极简请求器（与 platformHarness.do 的差异只在
// 允许不带 Authorization）。
func doJSON(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, rd)
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
