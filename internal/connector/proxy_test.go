package connector

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// proxy 注入相关的测试集中在这里；newTestManager / testKey 等 helper 在
// manager_test.go 里，同包可见。

func TestInjectProxySidecarBaseEmpty(t *testing.T) {
	m := newTestManager(t, func(c *Config) {
		c.InjectMode = InjectProxy
		c.SidecarBase = ""
	})
	env, err := m.Inject(t.Context(), "t1", "u1", []string{"openai"})
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("SidecarBase 为空应返回明确错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "出网代理未启用") || !strings.Contains(err.Error(), "proxy 注入不可用") {
		t.Fatalf("错误信息应说明代理未启用: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("不应返回可用地址，实际 %v", env)
	}
	for _, v := range env {
		if strings.Contains(v, "http://") || strings.Contains(v, "127.0.0.1") {
			t.Fatalf("不该给一个连不上的假地址: %v", env)
		}
	}
}

func TestAPIBaseEnvNameDerivation(t *testing.T) {
	cases := []struct{ env, want string }{
		{"GITHUB_TOKEN", "GITHUB_API_BASE"},
		{"GOOGLE_ACCESS_TOKEN", "GOOGLE_API_BASE"},
		{"NOTION_TOKEN", "NOTION_API_BASE"},
		{"OPENAI_API_KEY", "OPENAI_API_BASE"},
		{"SOMETHING_KEY", "SOMETHING_API_BASE"},
		{"CUSTOM", "CUSTOM_API_BASE"}, // 无后缀时兜底直接加 _API_BASE
	}
	for _, tc := range cases {
		if got := apiBaseEnvName(tc.env); got != tc.want {
			t.Errorf("apiBaseEnvName(%q)=%q，期望 %q", tc.env, got, tc.want)
		}
	}

	m := newTestManager(t, func(c *Config) { c.InjectMode = InjectProxy })
	env, err := m.Inject(t.Context(), "t1", "u1", []string{"github", "google", "notion", "openai"})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	want := map[string]string{
		"GITHUB_API_BASE": "http://127.0.0.1:18080/_connector/github",
		"GOOGLE_API_BASE": "http://127.0.0.1:18080/_connector/google",
		"NOTION_API_BASE": "http://127.0.0.1:18080/_connector/notion",
		"OPENAI_API_BASE": "http://127.0.0.1:18080/_connector/openai",
	}
	if len(env) != len(want) {
		t.Fatalf("变量数量不对: %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s=%q，期望 %q（env=%v）", k, env[k], v, env)
		}
	}
	if _, err := m.Inject(t.Context(), "t1", "u1", []string{"nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知连接器应返回 ErrNotFound，实际 %v", err)
	}
}

func TestBuiltinUpstreamInfo(t *testing.T) {
	m := newTestManager(t, nil)
	want := map[string]struct {
		hosts []string
		base  string
	}{
		"github": {[]string{"api.github.com"}, "https://api.github.com"},
		"google": {[]string{"www.googleapis.com", "gmail.googleapis.com"}, "https://www.googleapis.com"},
		"notion": {[]string{"api.notion.com"}, "https://api.notion.com"},
		"openai": {[]string{"api.openai.com"}, "https://api.openai.com"},
	}
	all := m.List()
	if len(all) != len(want) {
		t.Fatalf("内置连接器数量 %d，期望 %d", len(all), len(want))
	}
	for _, c := range all {
		w, ok := want[c.Name]
		if !ok {
			t.Fatalf("未知连接器 %q", c.Name)
		}
		if c.APIBase != w.base {
			t.Errorf("%s APIBase=%q，期望 %q", c.Name, c.APIBase, w.base)
		}
		if !slices.Equal(c.Hosts, w.hosts) {
			t.Errorf("%s Hosts=%v，期望 %v", c.Name, c.Hosts, w.hosts)
		}
		if !strings.HasPrefix(c.APIBase, "https://") {
			t.Errorf("%s APIBase 必须是 https: %s", c.Name, c.APIBase)
		}
		u, err := url.Parse(c.APIBase)
		if err != nil || !slices.Contains(c.Hosts, u.Host) {
			t.Errorf("%s APIBase 的域名必须在 Hosts 里: %s / %v", c.Name, c.APIBase, c.Hosts)
		}
	}
	// List() 返回的切片不能污染包级 definitions。
	for i := range all {
		if len(all[i].Hosts) > 0 {
			all[i].Hosts[0] = "tampered.example.com"
		}
	}
	for _, d := range definitions {
		if slices.Contains(d.Hosts, "tampered.example.com") {
			t.Fatal("List() 返回的 Hosts 与包级 definitions 共享底层数组")
		}
	}
}

func TestTokenForRoundTripAndUnauthorized(t *testing.T) {
	m := newTestManager(t, nil)
	const secret = "sk-roundtrip-secret"
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", secret); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	tok, err := m.TokenFor(t.Context(), "t1", "u1", "openai")
	if err != nil || tok != secret {
		t.Fatalf("TokenFor 往返失败: %q %v", tok, err)
	}

	// 未授权：必须是明确错误，不能是「空串 + nil」。
	tok, err = m.TokenFor(t.Context(), "t1", "u1", "github")
	if err == nil || tok != "" {
		t.Fatalf("未授权应返回明确错误: %q %v", tok, err)
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("未授权应包装 ErrNotConnected: %v", err)
	}
	if _, err := m.TokenFor(t.Context(), "t2", "u1", "openai"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("跨租户应返回 ErrNotConnected: %v", err)
	}
	off := newTestManager(t, func(c *Config) { c.Enabled = false })
	if _, err := off.TokenFor(t.Context(), "t1", "u1", "openai"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("未启用应返回 ErrDisabled: %v", err)
	}
}

func TestTokenForRefreshesAndCaches(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"gho_refreshed","refresh_token":"ghr_new","expires_in":3600,"scope":"repo"}`))
	}))
	defer srv.Close()

	m := newTestManager(t, nil)
	m.tokenURLs["github"] = srv.URL
	if err := m.vault.set(Credential{
		TenantID: "t1", UserID: "u1", Name: "github", Kind: KindOAuth2,
		Token: "gho_old", Refresh: "old-refresh",
		Expires: time.Now().Add(-time.Minute), Scopes: []string{"repo"},
		LinkedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("vault.set: %v", err)
	}

	tok, err := m.TokenFor(t.Context(), "t1", "u1", "github")
	if err != nil || tok != "gho_refreshed" {
		t.Fatalf("过期 token 应先刷新再返回: %q %v", tok, err)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("应只刷新一次，实际 %d", n)
	}
	// 缓存有效期内再取：不再刷新，仍返回同一 token。
	tok2, err := m.TokenFor(t.Context(), "t1", "u1", "github")
	if err != nil || tok2 != "gho_refreshed" {
		t.Fatalf("缓存命中失败: %q %v", tok2, err)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("缓存命中不应再刷新，实际 %d", n)
	}
	stored, err := m.vault.get("t1", "u1", "github")
	if err != nil || stored.Token != "gho_refreshed" {
		t.Fatalf("刷新后未落库: %+v err=%v", stored, err)
	}
}

func TestTokenForCacheInvalidatedOnWrite(t *testing.T) {
	m := newTestManager(t, nil)
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", "sk-1"); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	if tok, err := m.TokenFor(t.Context(), "t1", "u1", "openai"); err != nil || tok != "sk-1" {
		t.Fatalf("首次 TokenFor: %q %v", tok, err)
	}
	// 覆盖写：缓存必须失效，不能继续返回 sk-1。
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", "sk-2"); err != nil {
		t.Fatalf("PutKey 覆盖: %v", err)
	}
	if tok, err := m.TokenFor(t.Context(), "t1", "u1", "openai"); err != nil || tok != "sk-2" {
		t.Fatalf("PutKey 后缓存未失效: %q %v", tok, err)
	}
	// 删除：不能再返回旧 token（安全底线）。
	if err := m.Delete(t.Context(), "t1", "u1", "openai"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if tok, err := m.TokenFor(t.Context(), "t1", "u1", "openai"); err == nil || tok != "" {
		t.Fatalf("Delete 后仍返回旧 token: %q %v", tok, err)
	}
}

func TestTokenForConcurrent(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"gho_concurrent","refresh_token":"ghr_new","expires_in":3600,"scope":"repo"}`))
	}))
	defer srv.Close()

	m := newTestManager(t, nil)
	m.tokenURLs["github"] = srv.URL
	if err := m.vault.set(Credential{
		TenantID: "t1", UserID: "u1", Name: "github", Kind: KindOAuth2,
		Token: "gho_old", Refresh: "old-refresh",
		Expires: time.Now().Add(-time.Minute), Scopes: []string{"repo"},
		LinkedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("vault.set: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	tokCh := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := m.TokenFor(t.Context(), "t1", "u1", "github")
			if err != nil {
				errCh <- err
			} else {
				tokCh <- tok
			}
		}()
	}
	wg.Wait()
	close(errCh)
	close(tokCh)
	for err := range errCh {
		t.Errorf("并发 TokenFor 报错: %v", err)
	}
	for tok := range tokCh {
		if tok != "gho_concurrent" {
			t.Errorf("并发返回错误 token: %q", tok)
		}
	}
	// 单飞：并发只应刷新一次，避免重复刷新写库 / refresh token 轮换竞态。
	if got := refreshes.Load(); got != 1 {
		t.Errorf("并发应只刷新一次，实际 %d", got)
	}
	// vault 没被写坏：重启后仍能解出刷新后的 token。
	stored, err := m.vault.get("t1", "u1", "github")
	if err != nil || stored.Token != "gho_concurrent" {
		t.Fatalf("并发刷新后 vault 内容不对: %+v err=%v", stored, err)
	}
	reopened, err := Open(Config{Enabled: true, VaultKey: m.cfg.VaultKey, VaultPath: m.cfg.VaultPath})
	if err != nil {
		t.Fatalf("重启 Open: %v", err)
	}
	stored2, err := reopened.(*manager).vault.get("t1", "u1", "github")
	if err != nil || stored2.Token != "gho_concurrent" {
		t.Fatalf("重启后 token 损坏: %+v err=%v", stored2, err)
	}
}

func TestConnectorForHost(t *testing.T) {
	m := newTestManager(t, nil)
	cases := []struct{ host, want string }{
		{"api.github.com", "github"},
		{"API.GITHUB.COM", "github"},
		{"api.github.com:443", "github"},
		{"Api.GitHub.com:8443", "github"},
		{"api.github.com.", "github"},
		{"www.googleapis.com", "google"},
		{"gmail.googleapis.com", "google"},
		{"GMAIL.GOOGLEAPIS.COM:443", "google"},
		{"api.notion.com", "notion"},
		{"api.openai.com", "openai"},
		{"API.OPENAI.COM:443", "openai"},
	}
	for _, tc := range cases {
		c, ok := m.ConnectorForHost(tc.host)
		if !ok || c.Name != tc.want {
			t.Errorf("ConnectorForHost(%q)=%q,%v，期望 %q,true", tc.host, c.Name, ok, tc.want)
		}
	}
	unknown := []string{
		"", "github.com", "example.com", "evil-api.github.com",
		"api.github.com.evil.com", "notgoogleapis.com", "127.0.0.1:1234",
	}
	for _, h := range unknown {
		if c, ok := m.ConnectorForHost(h); ok {
			t.Errorf("ConnectorForHost(%q) 不该命中，实际 %q", h, c.Name)
		}
	}
	// 返回值不能污染包级 definitions。
	c, ok := m.ConnectorForHost("api.github.com")
	if !ok {
		t.Fatal("api.github.com 应命中")
	}
	c.Hosts[0] = "tampered.example.com"
	for _, d := range definitions {
		if d.Name == "github" && slices.Contains(d.Hosts, "tampered.example.com") {
			t.Fatal("ConnectorForHost 返回的 Hosts 与包级 definitions 共享底层数组")
		}
	}
}

func TestMatchPatternWildcard(t *testing.T) {
	cases := []struct {
		pat, host string
		want      bool
	}{
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false}, // 裸域不被 *. 命中
		{"*.example.com", "EXAMPLE.COM", false},
		{"*.example.com", "FOO.Example.COM", true},     // 大小写不敏感
		{"*.example.com", "foo.example.com:443", true}, // 带端口
		{"*.example.com:8443", "foo.example.com:443", true},
		{"*.example.com", "notexample.com", false},
		{"*.example.com", "example.com.evil.com", false},
		{"api.example.com", "api.example.com", true}, // 精确
		{"api.example.com", "API.EXAMPLE.COM:443", true},
		{"api.example.com", "x.api.example.com", false}, // 精确不含子域
		{"*", "anything.com", true},
		{"*example.com", "example.com", true}, // 后缀含裸域
		{"*example.com", "fooexample.com", true},
		{"", "foo.com", false},
		{"*.example.com", "", false},
	}
	for _, tc := range cases {
		if got := matchPattern(tc.pat, tc.host); got != tc.want {
			t.Errorf("matchPattern(%q, %q)=%v，期望 %v", tc.pat, tc.host, got, tc.want)
		}
	}
}

func TestConnectorForHostExactBeatsWildcard(t *testing.T) {
	defs := []definition{
		{Connector: Connector{Name: "wild", Hosts: []string{"*.example.com"}, APIBase: "https://wild.example.com"}},
		{Connector: Connector{Name: "exact", Hosts: []string{"api.example.com"}, APIBase: "https://api.example.com"}},
	}
	if c, ok := matchDefinitions("api.example.com", defs); !ok || c.Name != "exact" {
		t.Fatalf("精确应优先于通配: %q %v", c.Name, ok)
	}
	if c, ok := matchDefinitions("foo.example.com", defs); !ok || c.Name != "wild" {
		t.Fatalf("子域应命中通配: %q %v", c.Name, ok)
	}
	if _, ok := matchDefinitions("example.com", defs); ok {
		t.Fatal("裸域不该被 *. 命中")
	}
}
