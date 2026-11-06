package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// proxyClient 返回一个所有请求都走测试代理的客户端。
// DisableKeepAlives 是为了让「一次请求」对应「一次完整的连接生命周期」，
// 日志里的字节数才能在测试里稳定断言。
func proxyClient(t *testing.T, ctl Controller, user, pass string, tlsCfg *tls.Config) *http.Client {
	t.Helper()
	host := ctl.Addr()
	if user != "" {
		host = url.UserPassword(user, pass).String() + "@" + host
	}
	u, err := url.Parse("http://" + host)
	if err != nil {
		t.Fatalf("解析代理地址: %v", err)
	}
	tr := &http.Transport{
		Proxy:             http.ProxyURL(u),
		TLSClientConfig:   tlsCfg,
		DisableKeepAlives: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 8 * time.Second}
}

// waitEntry 轮询审计日志直到匹配，避免「连接刚关、日志还没回填」的时序抖动。
func waitEntry(t *testing.T, ctl Controller, tenant string, match func(LogEntry) bool) LogEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []LogEntry
	for {
		items, err := ctl.Log(context.Background(), tenant, 200)
		if err != nil {
			t.Fatalf("读日志: %v", err)
		}
		last = items
		for _, e := range items {
			if match(e) {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待日志超时，当前 %d 条: %+v", len(last), last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func allowLoopback() Policy {
	return Policy{
		Default:      Deny,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Allow}},
	}
}

func TestE2EHTTPAllowPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "hit")
		fmt.Fprintf(w, "ok:%s", r.URL.Path)
	}))
	defer srv.Close()

	ctl := openTestProxy(t, allowLoopback())
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatalf("走代理的请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok:/hello" {
		t.Fatalf("响应 = %d %q", resp.StatusCode, body)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Allow
	})
	if e.Reason != "rule:127.0.0.1" {
		t.Errorf("Reason = %q, want rule:127.0.0.1", e.Reason)
	}
	if e.TenantID != unownedTenant {
		t.Errorf("未带凭据的流量应标 %q，得到 %q", unownedTenant, e.TenantID)
	}
	if e.Port == "" {
		t.Error("日志缺少端口")
	}
}

func TestE2EHTTPDenyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("被拒绝的请求不该到达目标服务")
	}))
	defer srv.Close()

	ctl := openTestProxy(t, Policy{
		Default:      Allow,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Deny}},
	})
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(srv.URL + "/blocked")
	if err != nil {
		t.Fatalf("普通 HTTP 的拒绝应体现为 403 响应，而不是传输层错误: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want 403", resp.StatusCode)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Deny
	})
	if !strings.Contains(e.Reason, "rule:127.0.0.1") {
		t.Errorf("Reason = %q, 应指出命中规则", e.Reason)
	}
}

func TestE2EPrivateAddressBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("私网目标不该被连接")
	}))
	defer srv.Close()

	// 策略默认放行，但 AllowPrivate=false —— 私网拦截必须优先于策略。
	ctl := openTestProxy(t, Policy{Default: Allow, AllowPrivate: false})
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("私网拦截应返回 403 响应: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want 403", resp.StatusCode)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Deny
	})
	if e.Reason != "private-address" {
		t.Errorf("Reason = %q, want private-address", e.Reason)
	}
}

func TestE2EConnectAllowPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure-ok")
	}))
	defer srv.Close()

	ctl := openTestProxy(t, allowLoopback())
	client := proxyClient(t, ctl, "", "", &tls.Config{InsecureSkipVerify: true})

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("CONNECT 隧道请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "secure-ok" {
		t.Fatalf("响应 = %d %q", resp.StatusCode, body)
	}

	// 隧道结束后回填字节数：密文至少不比明文短。
	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Allow && e.BytesUp > 0 && e.BytesDown >= int64(len("secure-ok"))
	})
	if e.BytesUp == 0 {
		t.Error("CONNECT 上行字节数没统计到")
	}
}

func TestE2EConnectDenyPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("被拒绝的 CONNECT 不该到达目标")
	}))
	defer srv.Close()

	ctl := openTestProxy(t, Policy{
		Default:      Allow,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Deny}},
	})
	client := proxyClient(t, ctl, "", "", &tls.Config{InsecureSkipVerify: true})

	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("deny 时 CONNECT 应失败")
	}
	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Deny
	})
	if !strings.Contains(e.Reason, "rule:127.0.0.1") {
		t.Errorf("Reason = %q, 应指出命中规则", e.Reason)
	}
}

func TestE2EAskUnsupported(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("无 Approver 时 Ask 必须降级 deny，不该到达目标")
	}))
	defer srv.Close()

	// 没装 Approver：Ask 只能降级 Deny，绝不静默放行。
	ctl := openTestProxy(t, Policy{
		Default:      Deny,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Ask}},
	})
	// 登记一个带 runID 的沙箱，隔离出「缺的只是 Approver」这一个条件。
	ctl.Register("sb-ask", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-ask", "nonce", &tls.Config{InsecureSkipVerify: true})

	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("Ask 在无 Approver 时应表现为连接失败")
	}
	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Deny
	})
	if e.Reason != "ask-no-approver" {
		t.Errorf("Reason = %q, 应显式标 ask-no-approver", e.Reason)
	}
}

func TestE2EUnownedAndRegisteredTraffic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	ctl := openTestProxy(t, allowLoopback())
	// 已登记沙箱属于 t1；t1 自己的策略是全拒绝。
	if err := ctl.SetPolicy(context.Background(), "t1", Policy{Default: Deny, AllowPrivate: true}); err != nil {
		t.Fatal(err)
	}
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")

	// 已登记：按 t1 的策略拒绝，日志能追到 run。
	registered := proxyClient(t, ctl, "sb-1", "nonce-1", nil)
	resp, err := registered.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("已登记流量状态码 = %d, want 403", resp.StatusCode)
	}
	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool { return e.Decision == Deny })
	if e.RunID != "run-1" || e.UserID != "u1" || e.SessionID != "sess-1" {
		t.Errorf("归属信息不完整: %+v", e)
	}

	// 未登记：按默认策略放行，但必须标 unowned。
	unknown := proxyClient(t, ctl, "sb-unknown", "nonce", nil)
	resp2, err := unknown.Get(srv.URL)
	if err != nil {
		t.Fatalf("未登记请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("未登记流量应按默认策略放行，状态码 = %d", resp2.StatusCode)
	}
	e2 := waitEntry(t, ctl, unownedTenant, func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Allow
	})
	if e2.TenantID != unownedTenant {
		t.Errorf("TenantID = %q, want %q", e2.TenantID, unownedTenant)
	}
	if e2.Reason != "rule:127.0.0.1" {
		t.Errorf("Reason = %q, want rule:127.0.0.1（按默认策略的规则放行）", e2.Reason)
	}
}

func TestE2EUpstreamFailureIsLogged(t *testing.T) {
	// 占一个端口再立刻释放，保证目标端口确定是关闭的。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	ctl := openTestProxy(t, allowLoopback())
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get("http://" + deadAddr + "/")
	if err != nil {
		t.Fatalf("上游不可达应返回 502 响应: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, want 502", resp.StatusCode)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "127.0.0.1" && e.Decision == Deny
	})
	if e.Reason != "upstream-error" {
		t.Errorf("Reason = %q, want upstream-error", e.Reason)
	}
}

func TestE2EDNSResolvedPrivateBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("localhost 解析到私网，不该被连接")
	}))
	defer srv.Close()

	// 字面量不是 IP，decideHost 放行；拦截发生在 DNS 解析之后的拨号钩子里。
	ctl := openTestProxy(t, Policy{Default: Allow, AllowPrivate: false})
	client := proxyClient(t, ctl, "", "", nil)

	u := "http://localhost:" + portOf(t, srv.URL) + "/"
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want 403", resp.StatusCode)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Host == "localhost" && e.Decision == Deny
	})
	if e.Reason != "private-address" {
		t.Errorf("Reason = %q, want private-address", e.Reason)
	}
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestE2EByteCountsHTTP(t *testing.T) {
	const reqBody = "hello world"    // 11
	const respBody = "0123456789abc" // 13
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, respBody)
	}))
	defer srv.Close()

	ctl := openTestProxy(t, allowLoopback())
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != respBody {
		t.Fatalf("响应体 = %q", got)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool {
		return e.Decision == Allow && e.BytesUp >= int64(len(reqBody)) && e.BytesDown >= int64(len(respBody))
	})
	if e.BytesUp != int64(len(reqBody)) || e.BytesDown != int64(len(respBody)) {
		t.Errorf("字节统计 = up %d / down %d, want %d / %d",
			e.BytesUp, e.BytesDown, len(reqBody), len(respBody))
	}
}
