package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeTokens 是测试用的 TokenProvider：bases=name→基址，hosts=host→name。
// 字段在 Open 之前一次性设好，之后只读，handler goroutine 与测试 goroutine
// 之间不存在共享写，因此不需要加锁。
type fakeTokens struct {
	bases  map[string]string
	hosts  map[string]string
	header string
	value  string
	authOK bool
	tokErr error
}

func (f fakeTokens) TokenFor(ctx context.Context, tid, uid, host string) (string, string, bool, error) {
	if f.tokErr != nil {
		return "", "", false, f.tokErr
	}
	if !f.authOK {
		return "", "", false, nil
	}
	return f.header, f.value, true, nil
}
func (f fakeTokens) BaseFor(name string) (string, bool) { b, ok := f.bases[name]; return b, ok }
func (f fakeTokens) NameFor(host string) (string, bool) { n, ok := f.hosts[host]; return n, ok }

// openSidecarProxy 起一个带 TokenProvider 的代理，并把 sidecar 的转发 transport
// 换成信任 httptest 自签证书的版本（newProxy 里的生产版本走真实 https 校验）。
func openSidecarProxy(t *testing.T, tp TokenProvider) Controller {
	t.Helper()
	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = Policy{Default: Allow, AllowPrivate: true}
		c.TokenProvider = tp
	})
	ctl.(*proxy).sidecarHTTP = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return ctl
}

func sidecarURL(ctl Controller, path string) string {
	return "http://" + ctl.Addr() + path
}

// 主路径：header 注入、method/body/query 转发、响应回写、Proxy-Authorization
// 不外泄，并落一条 sidecar 审计。上游把收到的头回显到响应头，避免测试 goroutine
// 与 handler goroutine 共享变量产生数据竞争。
func TestSidecarInjectsTokenAndEchoes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Got-Proxy-Auth", r.Header.Get("Proxy-Authorization"))
		w.Header().Set("X-Got-Query", r.URL.RawQuery)
		w.Header().Set("X-Got-Method", r.Method)
		w.Header().Set("X-Got-Path", r.URL.Path)
		w.Header().Set("X-Upstream", "yes")
		fmt.Fprint(w, "ok:"+r.URL.Path)
	}))
	defer srv.Close()

	tp := fakeTokens{
		bases:  map[string]string{"github": srv.URL}, // https://127.0.0.1:port
		hosts:  map[string]string{"127.0.0.1": "github"},
		header: "Authorization",
		value:  "Bearer test-token",
		authOK: true,
	}
	ctl := openSidecarProxy(t, tp)
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", nil)

	resp, err := client.Get(sidecarURL(ctl, "/_connector/github/repos/foo?q=1&x=2"))
	if err != nil {
		t.Fatalf("sidecar 请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok:/repos/foo" {
		t.Fatalf("响应 = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("响应头没回写")
	}
	if got := resp.Header.Get("X-Got-Auth"); got != "Bearer test-token" {
		t.Errorf("注入的 header = %q, want Bearer test-token", got)
	}
	if got := resp.Header.Get("X-Got-Proxy-Auth"); got != "" {
		t.Errorf("Proxy-Authorization 泄漏给上游: %q", got)
	}
	if got := resp.Header.Get("X-Got-Query"); got != "q=1&x=2" {
		t.Errorf("query 未转发: %q", got)
	}
	if got := resp.Header.Get("X-Got-Method"); got != "GET" {
		t.Errorf("method 未转发: %q", got)
	}
	if got := resp.Header.Get("X-Got-Path"); got != "/repos/foo" {
		t.Errorf("路径不对: %q", got)
	}

	e := waitEntry(t, ctl, "", func(e LogEntry) bool { return e.Reason == "sidecar:github" })
	if e.Decision != Allow {
		t.Errorf("审计 decision = %s, want allow", e.Decision)
	}
}

func TestSidecarRejectsUnknownName(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("非法 name 不该到达上游")
	}))
	defer srv.Close()

	tp := fakeTokens{
		bases:  map[string]string{"github": srv.URL},
		hosts:  map[string]string{"127.0.0.1": "github"},
		header: "Authorization", value: "x", authOK: true,
	}
	ctl := openSidecarProxy(t, tp)
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(sidecarURL(ctl, "/_connector/unknown/repos/foo"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 name 状态码 = %d, want 400", resp.StatusCode)
	}
}

func TestSidecarRejectsNonHTTPSBase(t *testing.T) {
	// 基址是 http://（不是 https）：在拨号之前就该被拒，不需要真上游。
	tp := fakeTokens{
		bases:  map[string]string{"github": "http://api.example.com"},
		hosts:  map[string]string{"api.example.com": "github"},
		header: "Authorization", value: "x", authOK: true,
	}
	ctl := openSidecarProxy(t, tp)
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(sidecarURL(ctl, "/_connector/github/repos/foo"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非 https 基址状态码 = %d, want 400", resp.StatusCode)
	}
}

// 即便 BaseFor 因配置错误指向了任意 host，host 不在连接器 Hosts 里也必须拒。
func TestSidecarRejectsHostNotInHosts(t *testing.T) {
	tp := fakeTokens{
		bases:  map[string]string{"github": "https://evil.example.com"},
		hosts:  map[string]string{"api.github.com": "github"}, // evil 不在
		header: "Authorization", value: "x", authOK: true,
	}
	ctl := openSidecarProxy(t, tp)
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(sidecarURL(ctl, "/_connector/github/repos/foo"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("host 不属于 Hosts 状态码 = %d, want 400", resp.StatusCode)
	}
	e := waitEntry(t, ctl, "", func(e LogEntry) bool { return e.Reason == "bad-sidecar-host" })
	if e.Decision != Deny {
		t.Errorf("审计 decision = %s, want deny", e.Decision)
	}
}

func TestSidecarNoTokenReturns401(t *testing.T) {
	// TokenFor 返回 ok=false：必须 401，而不是转发一个没头的请求。
	tp := fakeTokens{
		bases: map[string]string{"github": "https://api.github.com"},
		hosts: map[string]string{"api.github.com": "github"},
		// authOK=false
	}
	ctl := openSidecarProxy(t, tp)
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(sidecarURL(ctl, "/_connector/github/repos/foo"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权状态码 = %d, want 401", resp.StatusCode)
	}
	e := waitEntry(t, ctl, "", func(e LogEntry) bool { return e.Reason == "no-token" })
	if e.Decision != Deny {
		t.Errorf("审计 decision = %s, want deny", e.Decision)
	}
}

// handleForward 的明文 HTTP absolute-URI 路径也按 host 注入 header。
func TestForwardInjectsTokenForPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Token", r.Header.Get("X-Api-Token"))
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tp := fakeTokens{header: "X-Api-Token", value: "secret", authOK: true}
	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = allowLoopback()
		c.TokenProvider = tp
	})
	client := proxyClient(t, ctl, "", "", nil)

	resp, err := client.Get(srv.URL + "/data")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Got-Token"); got != "secret" {
		t.Errorf("明文 HTTP 未注入 token, 上游收到 %q", got)
	}
}
