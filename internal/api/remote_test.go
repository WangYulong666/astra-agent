package api

// 远程桌面的 HTTP / WebSocket 层测试。
//
// 用一个手写的 fakeRemote 实现 remotedesk.Manager：这层要验的是「凭据怎么变成
// 归属」「错误怎么变成状态码」「WS 的帧怎么进出」，跟抓屏/注入的真实实现无关。
// 真实实现（R1/R2）有自己的单测，这里不再重复。
//
// 路由注册不复制 api.go 的那 12 行——主控的契约测试已经保证路由集合不漂，
// 这里只注册同名 handler 并手工注入 principal，跑得快也跑得稳。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/WangYulong666/astra-agent/internal/remotedesk"
	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/coder/websocket"
)

// remotePrincipal 是 A 租户 Owner。所有请求都以它的身份进来。
var remotePrincipal = auth.Principal{
	UserID:   "user-a",
	TenantID: "tenant-a",
	Email:    "a@astra.test",
	Role:     store.RoleOwner,
	Via:      auth.ViaJWT,
}

// ── 假 Manager ─────────────────────────────────────────────────

// remoteSeen 记录 manager 收到的归属，租户隔离的断言全靠它。
type remoteSeen struct {
	Method string
	Tenant string
	User   string
}

type fakeRemote struct {
	mu   sync.Mutex
	errs map[string]error
	seen []remoteSeen

	caps        remotedesk.Capabilities
	pairing     remotedesk.Pairing
	device      remotedesk.Device
	devices     []remotedesk.Device
	session     remotedesk.Session
	sessions    []remotedesk.Session
	actionRes   remotedesk.ActionResult
	lastPair    remotedesk.PairingRequest
	lastClaim   remotedesk.ClaimRequest
	lastStart   remotedesk.StartRequest
	lastAction  remotedesk.ActionRequest
	runtime     *fakeRuntime
	revokedID   string
	endedID     string
	endedReason string
}

func newFakeRemote() *fakeRemote {
	now := time.Now().UTC()
	return &fakeRemote{
		errs: map[string]error{},
		caps: remotedesk.Capabilities{Platform: "darwin", MaxFPS: 10, Notice: "只能看不能控"},
		pairing: remotedesk.Pairing{
			Code: "123456", ExpiresAt: now.Add(2 * time.Minute),
			Payload: "astra://pair?code=123456", DeepLink: "astra://remote/pair?code=123456",
		},
		device: remotedesk.Device{
			ID: "dev_1", Name: "Mac", Platform: "macos", CreatedAt: now, Online: true,
		},
		devices: []remotedesk.Device{{ID: "dev_1", Name: "Mac", Platform: "macos"}},
		session: remotedesk.Session{
			ID: "sess_1", DeviceID: "dev_1", Device: "Mac", Mode: remotedesk.ModeView,
			Status: remotedesk.StatusActive, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		sessions:  []remotedesk.Session{{ID: "sess_1", DeviceID: "dev_1", Mode: remotedesk.ModeView}},
		actionRes: remotedesk.ActionResult{Action: remotedesk.ActionLock, OK: true, Detail: "已锁屏"},
	}
}

func (f *fakeRemote) setErr(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, method)
		return
	}
	f.errs[method] = err
}

func (f *fakeRemote) errFor(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs[method]
}

func (f *fakeRemote) record(method, tenant, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, remoteSeen{Method: method, Tenant: tenant, User: user})
}

// lastFor 取某方法最后一次收到的归属。
func (f *fakeRemote) lastFor(method string) (tenant, user string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.seen) - 1; i >= 0; i-- {
		if f.seen[i].Method == method {
			return f.seen[i].Tenant, f.seen[i].User, true
		}
	}
	return "", "", false
}

func (f *fakeRemote) Capabilities(context.Context) (remotedesk.Capabilities, error) {
	if err := f.errFor("Capabilities"); err != nil {
		return remotedesk.Capabilities{}, err
	}
	return f.caps, nil
}

func (f *fakeRemote) StartPairing(_ context.Context, req remotedesk.PairingRequest) (remotedesk.Pairing, error) {
	f.record("StartPairing", req.TenantID, req.UserID)
	f.mu.Lock()
	f.lastPair = req
	f.mu.Unlock()
	if err := f.errFor("StartPairing"); err != nil {
		return remotedesk.Pairing{}, err
	}
	return f.pairing, nil
}

func (f *fakeRemote) ClaimPairing(_ context.Context, req remotedesk.ClaimRequest) (remotedesk.Device, error) {
	f.record("ClaimPairing", req.TenantID, req.UserID)
	f.mu.Lock()
	f.lastClaim = req
	f.mu.Unlock()
	if err := f.errFor("ClaimPairing"); err != nil {
		return remotedesk.Device{}, err
	}
	return f.device, nil
}

func (f *fakeRemote) ListDevices(_ context.Context, tenantID, userID string) ([]remotedesk.Device, error) {
	f.record("ListDevices", tenantID, userID)
	if err := f.errFor("ListDevices"); err != nil {
		return nil, err
	}
	return f.devices, nil
}

func (f *fakeRemote) RevokeDevice(_ context.Context, tenantID, userID, deviceID string) error {
	f.record("RevokeDevice", tenantID, userID)
	f.mu.Lock()
	f.revokedID = deviceID
	f.mu.Unlock()
	return f.errFor("RevokeDevice")
}

func (f *fakeRemote) StartSession(_ context.Context, req remotedesk.StartRequest) (remotedesk.Session, error) {
	f.record("StartSession", req.TenantID, req.UserID)
	f.mu.Lock()
	f.lastStart = req
	f.mu.Unlock()
	if err := f.errFor("StartSession"); err != nil {
		return remotedesk.Session{}, err
	}
	return f.session, nil
}

func (f *fakeRemote) GetSession(_ context.Context, tenantID, sessionID string) (remotedesk.Session, error) {
	f.record("GetSession", tenantID, "")
	if err := f.errFor("GetSession"); err != nil {
		return remotedesk.Session{}, err
	}
	s := f.session
	s.ID = sessionID
	return s, nil
}

func (f *fakeRemote) ListSessions(_ context.Context, tenantID string, limit int) ([]remotedesk.Session, error) {
	f.record("ListSessions", tenantID, "")
	if err := f.errFor("ListSessions"); err != nil {
		return nil, err
	}
	return f.sessions, nil
}

func (f *fakeRemote) ApproveSession(_ context.Context, tenantID, sessionID string) (remotedesk.Session, error) {
	f.record("ApproveSession", tenantID, "")
	if err := f.errFor("ApproveSession"); err != nil {
		return remotedesk.Session{}, err
	}
	s := f.session
	s.ID = sessionID
	s.Status = remotedesk.StatusActive
	return s, nil
}

func (f *fakeRemote) EndSession(_ context.Context, tenantID, sessionID, reason string) error {
	f.record("EndSession", tenantID, "")
	f.mu.Lock()
	f.endedID, f.endedReason = sessionID, reason
	f.mu.Unlock()
	return f.errFor("EndSession")
}

func (f *fakeRemote) Open(_ context.Context, tenantID, sessionID string) (remotedesk.Runtime, error) {
	f.record("Open", tenantID, "")
	if err := f.errFor("Open"); err != nil {
		return nil, err
	}
	if f.runtime == nil {
		return nil, errors.New("fakeRemote: 没配 runtime")
	}
	return f.runtime, nil
}

func (f *fakeRemote) RunAction(_ context.Context, req remotedesk.ActionRequest) (remotedesk.ActionResult, error) {
	f.record("RunAction", req.TenantID, req.UserID)
	f.mu.Lock()
	f.lastAction = req
	f.mu.Unlock()
	if err := f.errFor("RunAction"); err != nil {
		return remotedesk.ActionResult{}, err
	}
	return f.actionRes, nil
}

func (f *fakeRemote) Close() error { return nil }

// ── 假 Runtime ─────────────────────────────────────────────────

type fakeRuntime struct {
	frames    chan remotedesk.Frame
	inputs    chan remotedesk.InputEvent
	qualities chan remotedesk.Quality
	closed    chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	inputErr error
	stats    remotedesk.Stats
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		frames:    make(chan remotedesk.Frame, 4),
		inputs:    make(chan remotedesk.InputEvent, 4),
		qualities: make(chan remotedesk.Quality, 4),
		closed:    make(chan struct{}),
		stats:     remotedesk.Stats{FPS: 8, Width: 1280, Height: 720, Quality: 60},
	}
}

func (r *fakeRuntime) Frames(context.Context) (<-chan remotedesk.Frame, error) { return r.frames, nil }

func (r *fakeRuntime) Input(ev remotedesk.InputEvent) error {
	r.mu.Lock()
	err := r.inputErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r.inputs <- ev:
	default:
	}
	return nil
}

func (r *fakeRuntime) SetQuality(q remotedesk.Quality) error {
	select {
	case r.qualities <- q:
	default:
	}
	return nil
}

func (r *fakeRuntime) Stats() remotedesk.Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *fakeRuntime) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func (r *fakeRuntime) setInputErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputErr = err
}

// ── 脚手架 ─────────────────────────────────────────────────────

// newRemoteTestServer 起一个只带 remote 路由的 server，principal 固定为 A 租户。
// m 传 nil 用来测「未启用」。
func newRemoteTestServer(t *testing.T, m remotedesk.Manager) *httptest.Server {
	t.Helper()
	h := &handler{cfg: Config{Remote: m}, log: log.New(io.Discard, "", 0)}
	as := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			fn(w, r.WithContext(auth.WithPrincipal(r.Context(), remotePrincipal)))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/remote/capabilities", as(h.remoteCapabilities))
	mux.HandleFunc("POST /v1/remote/pairing", as(h.remoteStartPairing))
	mux.HandleFunc("POST /v1/remote/pairing/claim", as(h.remoteClaimPairing))
	mux.HandleFunc("GET /v1/remote/devices", as(h.remoteListDevices))
	mux.HandleFunc("DELETE /v1/remote/devices/{id}", as(h.remoteRevokeDevice))
	mux.HandleFunc("POST /v1/remote/sessions", as(h.remoteStartSession))
	mux.HandleFunc("GET /v1/remote/sessions", as(h.remoteListSessions))
	mux.HandleFunc("GET /v1/remote/sessions/{id}", as(h.remoteGetSession))
	mux.HandleFunc("POST /v1/remote/sessions/{id}/approve", as(h.remoteApproveSession))
	mux.HandleFunc("DELETE /v1/remote/sessions/{id}", as(h.remoteEndSession))
	mux.HandleFunc("GET /v1/remote/sessions/{id}/stream", as(h.remoteStream))
	mux.HandleFunc("POST /v1/remote/sessions/{id}/actions", as(h.remoteAction))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// remoteDo 发一个请求。upgrade=true 时带上 WS 握手头（用于走到 Accept 之前的
// 错误分支，不需要真的升级）。
func remoteDo(t *testing.T, srv *httptest.Server, method, path string, body any, upgrade bool) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// remoteEndpoint 是 12 条路由的测试元数据。errKeys 是「注入哪个 manager 方法」
// ——WS 那条会先后调 GetSession 与 Open，两个都注入同一种错误，保证不论谁先炸
// 状态码都对。
type remoteEndpoint struct {
	name    string
	method  string
	path    string
	body    any
	okCode  int
	errKeys []string
}

var remoteEndpoints = []remoteEndpoint{
	{name: "capabilities", method: "GET", path: "/v1/remote/capabilities", okCode: 200,
		errKeys: []string{"Capabilities"}},
	{name: "pairing", method: "POST", path: "/v1/remote/pairing",
		body: map[string]any{"device": "Mac"}, okCode: 201, errKeys: []string{"StartPairing"}},
	{name: "claim", method: "POST", path: "/v1/remote/pairing/claim",
		body: map[string]any{"code": "123456", "platform": "ios"}, okCode: 201, errKeys: []string{"ClaimPairing"}},
	{name: "devices", method: "GET", path: "/v1/remote/devices", okCode: 200,
		errKeys: []string{"ListDevices"}},
	{name: "revoke", method: "DELETE", path: "/v1/remote/devices/dev_1", okCode: 204,
		errKeys: []string{"RevokeDevice"}},
	{name: "session_start", method: "POST", path: "/v1/remote/sessions",
		body: map[string]any{"deviceId": "dev_1", "mode": "view"}, okCode: 201, errKeys: []string{"StartSession"}},
	{name: "session_list", method: "GET", path: "/v1/remote/sessions?limit=5", okCode: 200,
		errKeys: []string{"ListSessions"}},
	{name: "session_get", method: "GET", path: "/v1/remote/sessions/sess_1", okCode: 200,
		errKeys: []string{"GetSession"}},
	{name: "session_approve", method: "POST", path: "/v1/remote/sessions/sess_1/approve", okCode: 200,
		errKeys: []string{"ApproveSession"}},
	{name: "session_end", method: "DELETE", path: "/v1/remote/sessions/sess_1", okCode: 204,
		errKeys: []string{"EndSession"}},
	{name: "stream", method: "GET", path: "/v1/remote/sessions/sess_1/stream", okCode: 400,
		errKeys: []string{"GetSession", "Open"}},
	{name: "actions", method: "POST", path: "/v1/remote/sessions/sess_1/actions",
		body: map[string]any{"action": "lock", "args": map[string]string{}}, okCode: 200,
		errKeys: []string{"RunAction"}},
}

// ── 未启用 ─────────────────────────────────────────────────────

func assertRemoteJSON(t *testing.T, body []byte) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v（%s）", err, body)
	}
}

func TestRemoteDisabledReturns503(t *testing.T) {
	srv := newRemoteTestServer(t, nil)
	for _, ep := range remoteEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			code, body := remoteDo(t, srv, ep.method, ep.path, ep.body, false)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("%s 返回 %d，期望 503：%s", ep.path, code, body)
			}
			// 文案必须指向 capabilities：用户唯一的自救入口是那条接口。
			assertRemoteJSON(t, body)
			if !strings.Contains(string(body), "/v1/remote/capabilities") {
				t.Fatalf("503 文案没指向 capabilities：%s", body)
			}
		})
	}
}

// ── happy path ─────────────────────────────────────────────────

func TestRemoteHappyPaths(t *testing.T) {
	fr := newFakeRemote()
	srv := newRemoteTestServer(t, fr)

	for _, ep := range remoteEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			code, body := remoteDo(t, srv, ep.method, ep.path, ep.body, false)
			if code != ep.okCode {
				t.Fatalf("%s 返回 %d，期望 %d：%s", ep.path, code, ep.okCode, body)
			}
			if ep.okCode == http.StatusNoContent {
				if len(bytes.TrimSpace(body)) != 0 {
					t.Fatalf("204 不该有 body：%s", body)
				}
				return
			}
			assertRemoteJSON(t, body)
		})
	}

	// 形状契约：前端与 CLI 已经按这些字段写死了。
	_, body := remoteDo(t, srv, "POST", "/v1/remote/pairing", map[string]any{"device": "Mac"}, false)
	var pair struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
		Payload   string    `json:"payload"`
		DeepLink  string    `json:"deepLink"`
	}
	if err := json.Unmarshal(body, &pair); err != nil {
		t.Fatalf("配对响应不是 JSON: %v", err)
	}
	if pair.Code != "123456" || pair.Payload == "" {
		t.Fatalf("配对响应缺字段：%s", body)
	}

	_, body = remoteDo(t, srv, "GET", "/v1/remote/devices", nil, false)
	var list struct {
		Items []remotedesk.Device `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("设备列表形状不对：%s", body)
	}

	_, body = remoteDo(t, srv, "POST", "/v1/remote/sessions/sess_1/actions",
		map[string]any{"action": "lock"}, false)
	var res remotedesk.ActionResult
	if err := json.Unmarshal(body, &res); err != nil || !res.OK {
		t.Fatalf("快捷操作结果形状不对：%s", body)
	}

	// 空列表必须是 [] 而不是 null（前端直接 .map）。
	fr.sessions = nil
	_, body = remoteDo(t, srv, "GET", "/v1/remote/sessions", nil, false)
	if !strings.Contains(string(body), `"items":[]`) {
		t.Fatalf("空会话列表应是 []，实际：%s", body)
	}
}

// ── 错误映射 ───────────────────────────────────────────────────

func TestRemoteErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     int
		mustHave string
	}{
		{"not_found", fmt.Errorf("%w: 会话 x", remotedesk.ErrNotFound), 404, "不存在"},
		{"denied", fmt.Errorf("%w: 会话等待桌面端批准", remotedesk.ErrDenied), 403, "等待桌面端批准"},
		{"unavailable", fmt.Errorf("%w: 本机未授予屏幕录制权限", remotedesk.ErrUnavailable), 503, "屏幕录制"},
	}

	for _, ep := range remoteEndpoints {
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				fr := newFakeRemote()
				for _, k := range ep.errKeys {
					fr.setErr(k, c.err)
				}
				srv := newRemoteTestServer(t, fr)
				// stream 要走升级分支才会去碰 manager，否则先被 400 挡下。
				upgrade := ep.name == "stream"
				code, body := remoteDo(t, srv, ep.method, ep.path, ep.body, upgrade)
				if code != c.want {
					t.Fatalf("%s 返回 %d，期望 %d：%s", ep.path, code, c.want, body)
				}
				assertRemoteJSON(t, body)
				if !strings.Contains(string(body), c.mustHave) {
					t.Fatalf("响应没带出原因 %q：%s", c.mustHave, body)
				}
			})
		}
	}

	// 未知错误统一 500。
	t.Run("unknown_is_500", func(t *testing.T) {
		fr := newFakeRemote()
		fr.setErr("RunAction", errors.New("磁盘炸了"))
		srv := newRemoteTestServer(t, fr)
		code, body := remoteDo(t, srv, "POST", "/v1/remote/sessions/sess_1/actions",
			map[string]any{"action": "lock"}, false)
		if code != http.StatusInternalServerError {
			t.Fatalf("未知错误返回 %d，期望 500：%s", code, body)
		}
	})
}

// ── 租户隔离 ───────────────────────────────────────────────────

// TestRemoteTenantComesFromPrincipal：请求体与 query 里的 tenant 必须被完全忽略，
// manager 收到的只能是 principal 的 tenant+user。
func TestRemoteTenantComesFromPrincipal(t *testing.T) {
	fr := newFakeRemote()
	srv := newRemoteTestServer(t, fr)

	cases := []struct {
		method, path string
		body         any
		methodName   string
	}{
		{"POST", "/v1/remote/pairing?tenant=tenant-b",
			map[string]any{"device": "Mac", "tenant": "tenant-b", "tenantId": "tenant-b"}, "StartPairing"},
		{"POST", "/v1/remote/pairing/claim?tenant=tenant-b",
			map[string]any{"code": "123456", "tenant": "tenant-b"}, "ClaimPairing"},
		{"GET", "/v1/remote/devices?tenant=tenant-b", nil, "ListDevices"},
		{"DELETE", "/v1/remote/devices/dev_1?tenant=tenant-b", nil, "RevokeDevice"},
		{"POST", "/v1/remote/sessions?tenant=tenant-b",
			map[string]any{"deviceId": "dev_1", "mode": "view", "tenant": "tenant-b"}, "StartSession"},
		{"GET", "/v1/remote/sessions?tenant=tenant-b", nil, "ListSessions"},
		{"POST", "/v1/remote/sessions/sess_1/actions?tenant=tenant-b",
			map[string]any{"action": "lock", "args": map[string]string{"tenant": "tenant-b"}}, "RunAction"},
	}

	for _, c := range cases {
		t.Run(c.methodName, func(t *testing.T) {
			code, body := remoteDo(t, srv, c.method, c.path, c.body, false)
			if code < 200 || code > 299 {
				t.Fatalf("%s 返回 %d：%s", c.path, code, body)
			}
			tenant, user, ok := fr.lastFor(c.methodName)
			if !ok {
				t.Fatalf("%s 没被调用", c.methodName)
			}
			if tenant != remotePrincipal.TenantID {
				t.Fatalf("%s 收到 tenant=%q，期望 %q（请求体里的 tenant 必须被忽略）",
					c.methodName, tenant, remotePrincipal.TenantID)
			}
			// 不需要 userID 的方法（ListSessions/RevokeDevice 的读侧）由实现自己决定，
			// 但只要它收 userID，就必须是 principal 的。
			if user != "" && user != remotePrincipal.UserID {
				t.Fatalf("%s 收到 user=%q，期望 %q", c.methodName, user, remotePrincipal.UserID)
			}
		})
	}

	// 顺带确认请求体里的 tenant 没被透传进 manager 的请求结构。
	fr.mu.Lock()
	gotStart := fr.lastStart.TenantID
	gotAction := fr.lastAction.TenantID
	gotPair := fr.lastPair.TenantID
	fr.mu.Unlock()
	for name, got := range map[string]string{
		"StartSession": gotStart, "RunAction": gotAction, "StartPairing": gotPair,
	} {
		if got != remotePrincipal.TenantID {
			t.Fatalf("%s 的请求结构里 tenant=%q，期望 %q", name, got, remotePrincipal.TenantID)
		}
	}
}

// ── 入参校验 ───────────────────────────────────────────────────

func TestRemoteStartSessionRejectsUnknownMode(t *testing.T) {
	fr := newFakeRemote()
	srv := newRemoteTestServer(t, fr)

	for _, mode := range []string{"", "watch", "CONTROL"} {
		code, body := remoteDo(t, srv, "POST", "/v1/remote/sessions",
			map[string]any{"deviceId": "dev_1", "mode": mode}, false)
		if code != http.StatusBadRequest {
			t.Fatalf("mode=%q 返回 %d，期望 400：%s", mode, code, body)
		}
	}
	if _, _, ok := fr.lastFor("StartSession"); ok {
		t.Fatal("非法 mode 不该调用 manager")
	}

	// control 是合法的，且 requireApproval 要真的传下去。
	fr.session.Mode = remotedesk.ModeControl
	code, body := remoteDo(t, srv, "POST", "/v1/remote/sessions",
		map[string]any{"deviceId": "dev_1", "mode": "control", "requireApproval": false}, false)
	if code != http.StatusCreated {
		t.Fatalf("control 返回 %d：%s", code, body)
	}
	fr.mu.Lock()
	got := fr.lastStart
	fr.mu.Unlock()
	if got.RequireApproval == nil || *got.RequireApproval {
		t.Fatalf("requireApproval=false 没传下去：%+v", got.RequireApproval)
	}
}

func TestRemoteActionRequiresAction(t *testing.T) {
	srv := newRemoteTestServer(t, newFakeRemote())
	code, body := remoteDo(t, srv, "POST", "/v1/remote/sessions/sess_1/actions",
		map[string]any{"args": map[string]string{"x": "1"}}, false)
	if code != http.StatusBadRequest {
		t.Fatalf("缺 action 返回 %d，期望 400：%s", code, body)
	}
}

// ── 非升级请求 ─────────────────────────────────────────────────

// TestRemoteStreamRejectsPlainGET 是契约测试依赖的行为：普通 GET 必须回 400 JSON，
// 不能是 101、不能是纯文本、更不能 panic。
func TestRemoteStreamRejectsPlainGET(t *testing.T) {
	fr := newFakeRemote()
	fr.runtime = newFakeRuntime()
	srv := newRemoteTestServer(t, fr)

	code, body := remoteDo(t, srv, "GET", "/v1/remote/sessions/sess_1/stream", nil, false)
	if code != http.StatusBadRequest {
		t.Fatalf("普通 GET 返回 %d，期望 400：%s", code, body)
	}
	assertRemoteJSON(t, body)
	if !strings.Contains(string(body), "WebSocket") {
		t.Fatalf("400 文案没提 WebSocket：%s", body)
	}
	// 未升级就不该碰 manager（否则会白开一路抓屏泵）。
	if _, _, ok := fr.lastFor("Open"); ok {
		t.Fatal("非升级请求不该 Open")
	}
}

// ── 真连接 ─────────────────────────────────────────────────────

// TestRemoteWSStatsFrameShape 守住 stats 帧的扁平结构：Stats 是内嵌字段，
// 一旦有人给它加了 json tag（或改成具名字段），前端读 fps/width 就全变 undefined。
func TestRemoteWSStatsFrameShape(t *testing.T) {
	b, err := json.Marshal(remoteWSStats{Type: "stats", Stats: remotedesk.Stats{FPS: 8, Width: 1280}})
	if err != nil {
		t.Fatalf("序列化 stats 帧: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"type":"stats"`) || !strings.Contains(got, `"fps":8`) {
		t.Fatalf("stats 帧没有内联字段: %s", got)
	}
	if strings.Contains(got, `"Stats"`) {
		t.Fatalf("stats 帧被套了一层: %s", got)
	}
}

// TestRemoteOriginPatterns 默认放行本机回环，env 追加；绝不出现通配。
func TestRemoteOriginPatterns(t *testing.T) {
	t.Setenv("ASTRA_REMOTE_ORIGINS", " remote.example.test , https://app.example.test ")
	got := strings.Join(remoteOriginPatterns(), ",")
	for _, want := range []string{"localhost", "127.0.0.1", "remote.example.test", "https://app.example.test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("origin 允许列表缺 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "*") {
		// 单独一个 * 通配等于关掉 CSRF 防线；"localhost:*" 这种带端口的没问题。
		for _, p := range remoteOriginPatterns() {
			if p == "*" {
				t.Fatalf("origin 允许列表里出现了通配: %s", got)
			}
		}
	}
}

// TestRemoteStreamWebSocket 是本任务的核心测试：真的握手、真的收发帧。
func TestRemoteStreamWebSocket(t *testing.T) {
	fr := newFakeRemote()
	rt := newFakeRuntime()
	fr.runtime = rt
	srv := newRemoteTestServer(t, fr)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/remote/sessions/sess_1/stream"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}

	// 1. hello 必须是第一帧。
	typ, data := remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageText {
		t.Fatalf("hello 应该是 text 帧，实际 %v", typ)
	}
	var hello struct {
		Type      string           `json:"type"`
		SessionID string           `json:"sessionId"`
		Mode      string           `json:"mode"`
		Stats     remotedesk.Stats `json:"stats"`
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("hello 不是 JSON: %s", data)
	}
	if hello.Type != "hello" || hello.SessionID != "sess_1" || hello.Mode == "" {
		t.Fatalf("hello 内容不对: %s", data)
	}
	if hello.Stats.Width == 0 {
		t.Fatalf("hello 没带 stats: %s", data)
	}
	if tenant, _, _ := fr.lastFor("Open"); tenant != remotePrincipal.TenantID {
		t.Fatalf("Open 收到 tenant=%q，期望 %q", tenant, remotePrincipal.TenantID)
	}

	// 2. 服务端推两帧 → 客户端收到两条 binary，内容与顺序都要对。
	//    注意必须「推一帧、读一帧」：订阅通道缓冲只有 1 且满则丢最旧（产品
	//    设计如此，防慢客户端把抓屏泵拖死）。一次推两帧在 Linux CI 上会丢第 1 帧。
	want := [][]byte{{0xff, 0xd8, 0x01, 0x11}, {0xff, 0xd8, 0x02, 0x22}}
	for i, wantBytes := range want {
		rt.frames <- remotedesk.Frame{JPEG: wantBytes, Width: 1280, Height: 720, At: time.Now()}
		typ, data = remoteWSRead(t, ctx, conn)
		if typ != websocket.MessageBinary {
			t.Fatalf("第 %d 帧应该是 binary，实际 %v", i+1, typ)
		}
		if !bytes.Equal(data, wantBytes) {
			t.Fatalf("第 %d 帧内容不对：%v，期望 %v", i+1, data, wantBytes)
		}
	}

	// 3. 客户端发 input → fake 收到。
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"input","op":"click","x":0.5,"y":0.25}`)); err != nil {
		t.Fatalf("发送 input 失败: %v", err)
	}
	select {
	case ev := <-rt.inputs:
		if ev.Op != "click" || ev.X != 0.5 || ev.Y != 0.25 {
			t.Fatalf("input 解出来不对: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没收到 input")
	}

	// 4. input 被拒（限速/未授权）→ 只回 error 帧，连接必须还在。
	rt.setInputErr(fmt.Errorf("%w: 输入超速（上限 200/s）", remotedesk.ErrDenied))
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","op":"move"}`)); err != nil {
		t.Fatalf("发送第二个 input 失败: %v", err)
	}
	typ, data = remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageText {
		t.Fatalf("拒绝 input 应该回 text 帧，实际 %v", typ)
	}
	if !strings.Contains(string(data), "超速") {
		t.Fatalf("error 帧没带原因: %s", data)
	}
	rt.setInputErr(nil)

	// 5. 非法 JSON → error 帧，连接仍在。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{not json`)); err != nil {
		t.Fatalf("发送非法 JSON 失败: %v", err)
	}
	typ, data = remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageText || !strings.Contains(string(data), "JSON") {
		t.Fatalf("非法 JSON 应回 error 帧，实际 %v: %s", typ, data)
	}

	// 6. 连接仍然可用：ping → pong。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("发送 ping 失败: %v", err)
	}
	typ, data = remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageText || !strings.Contains(string(data), "pong") {
		t.Fatalf("ping 没回 pong，实际 %v: %s", typ, data)
	}

	// 7. quality 要传到 runtime。
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"quality","maxWidth":800,"fps":5,"quality":50}`)); err != nil {
		t.Fatalf("发送 quality 失败: %v", err)
	}
	select {
	case q := <-rt.qualities:
		if q.MaxWidth != 800 || q.FPS != 5 || q.Quality != 50 {
			t.Fatalf("quality 解出来不对: %+v", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没收到 quality")
	}

	// 8. 客户端断开 → runtime 必须被 Close（否则每次断线都漏一路订阅）。
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	select {
	case <-rt.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后 runtime 没有被 Close")
	}
}

func remoteWSRead(t *testing.T, ctx context.Context, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读 WS 消息失败: %v", err)
	}
	return typ, data
}

// ── 审计 ───────────────────────────────────────────────────────

type recordingAuditor struct {
	mu   sync.Mutex
	logs []store.AuditLog
}

func (a *recordingAuditor) Audit(_ context.Context, l store.AuditLog) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, l)
}

func (a *recordingAuditor) byAction(action string) (store.AuditLog, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.logs {
		if l.Action == action {
			return l, true
		}
	}
	return store.AuditLog{}, false
}

// TestRemoteAuditOmitsSecrets：配对码、URL 的 query 绝不能进审计 detail。
// 审计表给很多人看，配对码写进去等于把「绑一台手机」的凭据公开了。
func TestRemoteAuditOmitsSecrets(t *testing.T) {
	fr := newFakeRemote()
	aud := &recordingAuditor{}

	h := &handler{cfg: Config{Remote: fr, Auditor: aud}, log: log.New(io.Discard, "", 0)}
	as := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			fn(w, r.WithContext(auth.WithPrincipal(r.Context(), remotePrincipal)))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/remote/pairing", as(h.remoteStartPairing))
	mux.HandleFunc("POST /v1/remote/pairing/claim", as(h.remoteClaimPairing))
	mux.HandleFunc("POST /v1/remote/sessions/{id}/actions", as(h.remoteAction))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if code, body := remoteDo(t, srv, "POST", "/v1/remote/pairing",
		map[string]any{"device": "Mac"}, false); code != http.StatusCreated {
		t.Fatalf("配对返回 %d：%s", code, body)
	}
	if code, body := remoteDo(t, srv, "POST", "/v1/remote/pairing/claim",
		map[string]any{"code": "123456"}, false); code != http.StatusCreated {
		t.Fatalf("认领返回 %d：%s", code, body)
	}
	if code, body := remoteDo(t, srv, "POST", "/v1/remote/sessions/sess_1/actions",
		map[string]any{"action": "open_url", "args": map[string]string{
			"url": "https://example.test/cb?token=SECRET-TOKEN",
		}}, false); code != http.StatusOK {
		t.Fatalf("快捷操作返回 %d：%s", code, body)
	}

	pair, ok := aud.byAction("remote.pair")
	if !ok {
		t.Fatal("没有 remote.pair 审计")
	}
	if strings.Contains(string(pair.Detail), "123456") || strings.Contains(string(pair.Detail), "code") {
		t.Fatalf("配对审计里出现了配对码：%s", pair.Detail)
	}

	claim, ok := aud.byAction("remote.claim")
	if !ok {
		t.Fatal("没有 remote.claim 审计")
	}
	if strings.Contains(string(claim.Detail), "123456") {
		t.Fatalf("认领审计里出现了配对码：%s", claim.Detail)
	}

	act, ok := aud.byAction("remote.action")
	if !ok {
		t.Fatal("没有 remote.action 审计")
	}
	if strings.Contains(string(act.Detail), "SECRET-TOKEN") || strings.Contains(string(act.Detail), "token=") {
		t.Fatalf("快捷操作审计里出现了 URL query：%s", act.Detail)
	}
	if !strings.Contains(string(act.Detail), "example.test") {
		t.Fatalf("open_url 审计应留下 host：%s", act.Detail)
	}
}
