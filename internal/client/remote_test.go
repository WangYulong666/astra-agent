package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 远程桌面的协议翻译测试：method / path / query / body / 解析。
// 全部用 httptest，不打真控制面。204 类的命令单独验「不解析响应体」，
// 因为服务端确实是空 body——这条最容易在改动里被写成解析失败。

type remoteProbe struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func newRemoteServer(t *testing.T, status int, body string, p *remoteProbe) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		p.method, p.path, p.query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		p.auth, p.body = r.Header.Get("Authorization"), string(raw)
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// jsonBody 把请求体解成 map 方便断言字段，避免依赖编码顺序。
func jsonBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("请求体不是合法 JSON：%s（%v）", raw, err)
	}
	return m
}

func TestRemoteCapabilities(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{
		"platform":"darwin",
		"capture":{"available":true,"backend":"screencapture"},
		"input":{"available":false,"reason":"辅助功能未授权","hint":"系统设置 → 隐私与安全 → 辅助功能"},
		"actions":{"available":true,"backend":"open"},
		"screen":{"width":1920,"height":1080},
		"maxFps":10,
		"notice":"本机未授予辅助功能权限，只能看不能控"}`, &p)
	defer srv.Close()

	caps, err := New(srv.URL, "tok").RemoteCapabilities(context.Background())
	if err != nil {
		t.Fatalf("RemoteCapabilities: %v", err)
	}
	if p.method != http.MethodGet || p.path != "/v1/remote/capabilities" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	if p.auth != "Bearer tok" {
		t.Fatalf("缺 Bearer 头：%q", p.auth)
	}
	if !caps.Capture.Available || caps.Input.Available {
		t.Fatalf("三层可用性解析错了：%+v", caps)
	}
	if caps.MaxFPS != 10 || caps.Screen.Width != 1920 || caps.Screen.Height != 1080 {
		t.Fatalf("screen/maxFps 解析错了：%+v", caps)
	}
	if caps.Input.Reason == "" || caps.Input.Hint == "" {
		t.Fatalf("不可用层必须带原因与修复提示：%+v", caps.Input)
	}
	if caps.Notice == "" {
		t.Fatalf("notice 丢了：%+v", caps)
	}
}

func TestRemotePairSendsDevice(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 201, `{"code":"123456","expiresAt":"2026-11-10T10:00:00Z","payload":"astra://remote/pair?code=123456","deepLink":"astra://remote/pair?code=123456&server=http://127.0.0.1:7788"}`, &p)
	defer srv.Close()

	pair, err := New(srv.URL, "tok").RemotePair(context.Background(), "我的 MacBook")
	if err != nil {
		t.Fatalf("RemotePair: %v", err)
	}
	if p.method != http.MethodPost || p.path != "/v1/remote/pairing" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	body := jsonBody(t, p.body)
	if body["device"] != "我的 MacBook" {
		t.Fatalf("device 没传出去：%v", body)
	}
	if pair.Code != "123456" || pair.Payload == "" || pair.DeepLink == "" {
		t.Fatalf("配对响应解析错了：%+v", pair)
	}
	if got := pair.ExpiresAt.UTC().Format(time.RFC3339); got != "2026-11-10T10:00:00Z" {
		t.Fatalf("expiresAt 解析错了：%s", got)
	}
}

func TestRemotePairOmitsEmptyDevice(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 201, `{"code":"000001","expiresAt":"2026-11-10T10:00:00Z","payload":"x"}`, &p)
	defer srv.Close()
	if _, err := New(srv.URL, "tok").RemotePair(context.Background(), ""); err != nil {
		t.Fatalf("RemotePair: %v", err)
	}
	if _, ok := jsonBody(t, p.body)["device"]; ok {
		t.Fatalf("device 为空时不该出现该字段（服务端会用 hostname）：%s", p.body)
	}
}

func TestRemotePairingRemaining(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	p := RemotePairing{ExpiresAt: now.Add(90 * time.Second)}
	if got := p.Remaining(now); got != 90*time.Second {
		t.Fatalf("剩余时间算错：%v", got)
	}
	if got := p.Remaining(now.Add(200 * time.Second)); got != 0 {
		t.Fatalf("过期后应返回 0，实际 %v", got)
	}
}

func TestRemoteClaim(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 201, `{"id":"dev1","name":"iPhone","platform":"ios","createdAt":"2026-11-10T10:00:00Z","lastSeenAt":"2026-11-10T10:00:05Z","online":true}`, &p)
	defer srv.Close()

	dev, err := New(srv.URL, "tok").RemoteClaim(context.Background(), "123456", "iPhone")
	if err != nil {
		t.Fatalf("RemoteClaim: %v", err)
	}
	if p.method != http.MethodPost || p.path != "/v1/remote/pairing/claim" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	body := jsonBody(t, p.body)
	if body["code"] != "123456" || body["device"] != "iPhone" {
		t.Fatalf("body 不对：%v", body)
	}
	if dev.ID != "dev1" || !dev.Online || dev.RevokedAt != nil {
		t.Fatalf("设备解析错了：%+v", dev)
	}
}

func TestRemoteClaimRequiresCode(t *testing.T) {
	// 空 code 必须在本地挡掉：发出去必然 400，还会刷一条失败审计。
	_, err := New("http://127.0.0.1:1", "tok").RemoteClaim(context.Background(), "  ", "")
	if !IsUseError(err) {
		t.Fatalf("空配对码应是用法错误，实际 %v", err)
	}
}

func TestRemoteDevices(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"items":[
		{"id":"d1","name":"Mac","platform":"darwin","online":true,"revokedAt":null},
		{"id":"d2","name":"旧手机","platform":"ios","online":false,"revokedAt":"2026-11-09T01:00:00Z"}]}`, &p)
	defer srv.Close()

	items, err := New(srv.URL, "tok").RemoteDevices(context.Background())
	if err != nil {
		t.Fatalf("RemoteDevices: %v", err)
	}
	if p.method != http.MethodGet || p.path != "/v1/remote/devices" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	if len(items) != 2 || items[0].Name != "Mac" || items[1].RevokedAt == nil {
		t.Fatalf("设备列表解析错了：%+v", items)
	}
}

func TestRemoteDevicesEmptyIsSliceNotNil(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"items":null}`, &p)
	defer srv.Close()
	items, err := New(srv.URL, "tok").RemoteDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("空结果应是空切片，实际 %#v", items)
	}
}

func TestRemoteRevokeDevice204(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, http.StatusNoContent, "", &p)
	defer srv.Close()

	if err := New(srv.URL, "tok").RemoteRevokeDevice(context.Background(), "dev/1"); err != nil {
		t.Fatalf("204 不该报错：%v", err)
	}
	if p.method != http.MethodDelete {
		t.Fatalf("应为 DELETE，实际 %s", p.method)
	}
	// PathEscape 保证 id 里的斜杠不会跑到别的路由上去。
	if p.path != "/v1/remote/devices/dev%2F1" {
		t.Fatalf("设备 id 未转义：%s", p.path)
	}
}

func TestRemoteStartSessionPassesMode(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 201, `{"id":"s1","deviceId":"d1","device":"Mac","mode":"control","status":"pending_approval","createdAt":"2026-11-10T10:00:00Z","expiresAt":"2026-11-10T14:00:00Z","stats":{"fps":0,"width":0,"height":0,"quality":0,"frames":0,"dropped":0,"inputs":0}}`, &p)
	defer srv.Close()

	sess, err := New(srv.URL, "tok").RemoteStartSession(context.Background(), "d1", RemoteModeControl)
	if err != nil {
		t.Fatalf("RemoteStartSession: %v", err)
	}
	if p.method != http.MethodPost || p.path != "/v1/remote/sessions" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	body := jsonBody(t, p.body)
	if body["deviceId"] != "d1" || body["mode"] != "control" {
		t.Fatalf("deviceId/mode 没透传：%v", body)
	}
	if sess.Status != RemoteStatusPending || sess.Mode != RemoteModeControl || sess.Device != "Mac" {
		t.Fatalf("会话解析错了：%+v", sess)
	}
}

func TestRemoteSessionsLimit(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"items":[{"id":"s1","deviceId":"d1","mode":"view","status":"active","stats":{"fps":7.5,"dropped":3}}]}`, &p)
	defer srv.Close()

	items, err := New(srv.URL, "tok").RemoteSessions(context.Background(), 50)
	if err != nil {
		t.Fatalf("RemoteSessions: %v", err)
	}
	if p.path != "/v1/remote/sessions" || !strings.Contains(p.query, "limit=50") {
		t.Fatalf("limit 没带出去：%s?%s", p.path, p.query)
	}
	if len(items) != 1 || items[0].Stats.FPS != 7.5 || items[0].Stats.Dropped != 3 {
		t.Fatalf("stats 解析错了：%+v", items)
	}
}

func TestRemoteSessionsDefaultLimit(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"items":[]}`, &p)
	defer srv.Close()
	if _, err := New(srv.URL, "tok").RemoteSessions(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.query, "limit=20") {
		t.Fatalf("默认 limit 应为 20：%s", p.query)
	}
}

func TestRemoteSessionGet(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"id":"s1","deviceId":"d1","mode":"view","status":"active","stats":{"fps":9.5}}`, &p)
	defer srv.Close()

	sess, err := New(srv.URL, "tok").RemoteSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("RemoteSession: %v", err)
	}
	if p.method != http.MethodGet || p.path != "/v1/remote/sessions/s1" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	if sess.ID != "s1" || sess.Stats.FPS != 9.5 {
		t.Fatalf("会话解析错了：%+v", sess)
	}
}

func TestRemoteApprove(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"id":"s1","mode":"control","status":"active","approvedAt":"2026-11-10T10:00:10Z","stats":{}}`, &p)
	defer srv.Close()

	sess, err := New(srv.URL, "tok").RemoteApprove(context.Background(), "s1")
	if err != nil {
		t.Fatalf("RemoteApprove: %v", err)
	}
	if p.method != http.MethodPost || p.path != "/v1/remote/sessions/s1/approve" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	if sess.Status != RemoteStatusActive || sess.ApprovedAt == nil {
		t.Fatalf("批准后状态解析错了：%+v", sess)
	}
}

func TestRemoteEndSession204(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, http.StatusNoContent, "", &p)
	defer srv.Close()

	if err := New(srv.URL, "tok").RemoteEndSession(context.Background(), "s1"); err != nil {
		t.Fatalf("204 不该报错：%v", err)
	}
	if p.method != http.MethodDelete || p.path != "/v1/remote/sessions/s1" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
}

func TestRemoteActionArgs(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"action":"open_url","ok":true,"detail":"已提交"}}`, &p)
	defer srv.Close()

	res, err := New(srv.URL, "tok").RemoteAction(context.Background(), "s1", RemoteActionOpenURL,
		map[string]string{"url": "https://example.com/x"})
	if err != nil {
		t.Fatalf("RemoteAction: %v", err)
	}
	if p.method != http.MethodPost || p.path != "/v1/remote/sessions/s1/actions" {
		t.Fatalf("请求不对：%s %s", p.method, p.path)
	}
	body := jsonBody(t, p.body)
	if body["action"] != "open_url" {
		t.Fatalf("action 没透传：%v", body)
	}
	args, ok := body["args"].(map[string]any)
	if !ok || args["url"] != "https://example.com/x" {
		t.Fatalf("args 没透传：%v", body["args"])
	}
	if !res.OK || res.Action != "open_url" {
		t.Fatalf("结果解析错了：%+v", res)
	}
}

func TestRemoteActionScreenshotPath(t *testing.T) {
	var p remoteProbe
	srv := newRemoteServer(t, 200, `{"action":"screenshot","ok":true,"path":"/data/remote/shots/1.png"}`, &p)
	defer srv.Close()

	res, err := New(srv.URL, "tok").RemoteAction(context.Background(), "s1", RemoteActionScreenshot, nil)
	if err != nil {
		t.Fatalf("RemoteAction: %v", err)
	}
	if res.Path != "/data/remote/shots/1.png" {
		t.Fatalf("path 丢了：%+v", res)
	}
	// args 为 nil 时不该发出一个空对象，服务端对两都接受，但空字段是噪音。
	if _, ok := jsonBody(t, p.body)["args"]; ok {
		t.Fatalf("args 为 nil 时不该带该字段：%s", p.body)
	}
}

func TestRemoteActionRequiresAction(t *testing.T) {
	_, err := New("http://127.0.0.1:1", "tok").RemoteAction(context.Background(), "s1", "", nil)
	if !IsUseError(err) {
		t.Fatalf("空 action 应是用法错误，实际 %v", err)
	}
}

func TestRemoteAPIErrorSurfaces(t *testing.T) {
	// 503 是「能力不可用」的常态（缺权限 / 全局急停），必须原样把原因带上来，
	// 不能被包成一句「请求失败」。
	var p remoteProbe
	srv := newRemoteServer(t, http.StatusServiceUnavailable,
		`{"error":"远程桌面未启用（ASTRA_REMOTE_DISABLED 或探测失败）"}`, &p)
	defer srv.Close()

	_, err := New(srv.URL, "tok").RemoteCapabilities(context.Background())
	if !IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("应保留 503 语义，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "ASTRA_REMOTE_DISABLED") {
		t.Fatalf("服务端原因被吞了：%v", err)
	}
}
