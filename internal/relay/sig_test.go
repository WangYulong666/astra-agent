package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialTunnel 只用来看「握手结果」，不关心连接是否留下来。
// 返回 HTTP 状态码：101 表示通过（升级成功）。
func dialTunnelStatus(t *testing.T, rawURL string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, rawURL, nil)
	if c != nil {
		_ = c.CloseNow()
	}
	if err != nil && resp == nil {
		t.Fatalf("Dial %s: %v", rawURL, err)
	}
	return resp.StatusCode
}

func tunnelURL(addr, device string, ts int64, sig string) string {
	return fmt.Sprintf("ws://%s/relay/tunnel?device=%s&ts=%d&sig=%s", addr, device, ts, sig)
}

func TestTunnelSignature(t *testing.T) {
	const token = "top-secret"
	srv := newTestServer(t, ServerConfig{Token: token})
	now := time.Now().Unix()
	good := signTicket(token, "dev-1", strconv.FormatInt(now, 10))

	cases := []struct {
		name   string
		device string
		ts     int64
		sig    string
		want   int
	}{
		{"正确签名", "dev-1", now, good, http.StatusSwitchingProtocols},
		{"错误签名", "dev-1", now, strings.Repeat("a", 64), http.StatusUnauthorized},
		{"时间戳偏差 130s", "dev-1", now - 130, signTicket(token, "dev-1", strconv.FormatInt(now-130, 10)), http.StatusUnauthorized},
		{"时间戳超前 130s", "dev-1", now + 130, signTicket(token, "dev-1", strconv.FormatInt(now+130, 10)), http.StatusUnauthorized},
		{"device 与签名不匹配", "dev-2", now, good, http.StatusUnauthorized},
		{"缺 sig", "dev-1", now, "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dialTunnelStatus(t, tunnelURL(srv.Addr(), tc.device, tc.ts, tc.sig))
			if got != tc.want {
				t.Fatalf("状态码 = %d, 期望 %d", got, tc.want)
			}
		})
	}
}

func TestTunnelRejectsBadDeviceID(t *testing.T) {
	srv := newTestServer(t, ServerConfig{Token: "tk"})
	// 路径注入 / 日志注入的常见写法都必须挡住。
	for _, dev := range []string{"", "a/b", "../etc", "has space", strings.Repeat("x", 65)} {
		ts := time.Now().Unix()
		url := tunnelURL(srv.Addr(), dev, ts, signTicket("tk", dev, strconv.FormatInt(ts, 10)))
		code := dialTunnelStatus(t, url)
		if code == http.StatusSwitchingProtocols {
			t.Fatalf("device=%q 不该被接受", dev)
		}
	}
}

// 调试模式（无 token）：非回环来源一律 403，回环放行。
func TestDebugModeLoopbackOnly(t *testing.T) {
	srv := newTestServer(t, ServerConfig{Token: ""}) // 无 token = 调试模式
	ts := time.Now().Unix()

	// 回环：通过（无 sig 也行）
	code := dialTunnelStatus(t, tunnelURL(srv.Addr(), "dev-1", ts, ""))
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("回环来源应放行，实际 %d", code)
	}

	// 伪造来源：把 RemoteAddr 换成公网地址再进同一个 handler。
	h := srv.Handler()
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "203.0.113.9:54321"
		h.ServeHTTP(w, r)
	})
	ts2 := httptest.NewServer(fake)
	defer ts2.Close()

	url := strings.Replace(tunnelURL(srv.Addr(), "dev-2", ts, ""), "ws://"+srv.Addr(), "ws://"+strings.TrimPrefix(ts2.URL, "http://"), 1)
	code = dialTunnelStatus(t, url)
	if code != http.StatusForbidden {
		t.Fatalf("非回环来源应 403，实际 %d", code)
	}
}

// 伪造 X-Forwarded-For 不能绕过调试模式的回环检查。
func TestDebugModeIgnoresForwardedFor(t *testing.T) {
	srv := newTestServer(t, ServerConfig{Token: ""})
	h := srv.Handler()
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "203.0.113.9:54321"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(fake)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := ts.URL + "/relay/tunnel?device=evil&ts=1&sig="
	url = strings.Replace(url, "http://", "ws://", 1)
	_, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil || resp == nil {
		t.Fatalf("期望被拒绝，err=%v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("X-Forwarded-For 伪造不该被信任，状态码 %d", resp.StatusCode)
	}
}

// 客户端自带的 X-Forwarded-For 必须被丢掉，换成真实来源。
func TestProxyStripsClientForwardedFor(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk"})
	newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)

	req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/t/dev-1/hello", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := decodeJSON(resp.Body, &m); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	xff, _ := m["xff"].(string)
	if strings.Contains(xff, "1.2.3.4") {
		t.Fatalf("伪造的 X-Forwarded-For 被透传了: %q", xff)
	}
	if !strings.HasPrefix(xff, "127.0.0.1") {
		t.Fatalf("X-Forwarded-For 应为真实来源 127.0.0.1，实际 %q", xff)
	}
	t.Logf("origin 收到的 X-Forwarded-For：%q", xff)
}

// 设备重复注册：新会话顶掉旧会话，注册表里只留一个，且旧连接被关。
func TestDuplicateDeviceReplacesSession(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk"})
	old := newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)

	srv.mu.RLock()
	first := srv.devices["dev-1"]
	srv.mu.RUnlock()

	fresh := newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)
	// 旧客户端会一直自动重连并再次顶号，测试里先让它退场。
	defer old.Close()

	waitFor(t, "注册表换成新会话", 2*time.Second, func() bool {
		srv.mu.RLock()
		cur := srv.devices["dev-1"]
		srv.mu.RUnlock()
		return cur != nil && cur != first
	})
	// 换表和关旧会话是同一个协程里的两步，中间有极短的窗口，只能轮询。
	waitFor(t, "旧会话被关闭", 2*time.Second, func() bool { return first.sess.IsClosed() })
	if !fresh.Stats().Connected {
		t.Fatal("新会话应处于已连接状态")
	}
	waitFor(t, "经新会话可访问", 2*time.Second, func() bool {
		code, _, _ := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hello")
		return code == http.StatusOK
	})
}
