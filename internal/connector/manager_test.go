package connector

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManager 按真实装配路径（Open）构造 manager，避免测试里手搓结构体
// 把生产路径上的校验（key 长度、注入模式、vault 加载）绕过去。
func newTestManager(t *testing.T, mutate func(*Config)) *manager {
	t.Helper()
	cfg := Config{
		Enabled:            true,
		VaultKey:           base64.StdEncoding.EncodeToString(testKey(t)),
		VaultPath:          filepath.Join(t.TempDir(), "vault.json"),
		BaseURL:            "https://astra.example.com",
		GitHubClientID:     "gh-id",
		GitHubClientSecret: "gh-secret",
		GoogleClientID:     "goog-id",
		GoogleClientSecret: "goog-secret",
		NotionClientID:     "no-id",
		NotionClientSecret: "no-secret",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	got, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, ok := got.(*manager)
	if !ok {
		t.Fatalf("Open 返回的不是 *manager: %T", got)
	}
	return m
}

func TestOpenRejectsBadVaultKey(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		key  string
	}{
		{"空", ""},
		{"16 字节", base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"31 字节", base64.StdEncoding.EncodeToString(make([]byte, 31))},
		{"33 字节", base64.StdEncoding.EncodeToString(make([]byte, 33))},
		{"不是 base64", "not*base64*at*all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(Config{
				Enabled:   true,
				VaultKey:  tc.key,
				VaultPath: filepath.Join(dir, "vault.json"),
			})
			if err == nil {
				t.Fatal("key 不合法时 Open 必须报错（否则等于明文存 token）")
			}
		})
	}
}

func TestOpenDisabled(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.Enabled = false })
	if m.Enabled() {
		t.Fatal("Enabled=false 时 Enabled() 应为 false")
	}
	if _, err := m.ListCredentials(nil, "t1", "u1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("应为 ErrDisabled，实际 %v", err)
	}
	if len(m.List()) != 4 {
		t.Fatalf("List 仍应返回四个内置连接器（只是不可连接）")
	}
}

func TestOpenRejectsUnknownInjectMode(t *testing.T) {
	_, err := Open(Config{
		Enabled:    true,
		VaultKey:   base64.StdEncoding.EncodeToString(testKey(t)),
		VaultPath:  filepath.Join(t.TempDir(), "vault.json"),
		InjectMode: "carrier-pigeon",
	})
	if err == nil {
		t.Fatal("未知注入模式应报错，不能悄悄按默认值跑")
	}
}

func TestOpenAcceptsRawURLBase64Key(t *testing.T) {
	key := testKey(t)
	// 不带 padding 的 url 形式（密钥管理系统里常见）。
	raw := base64.RawURLEncoding.EncodeToString(key)
	if _, err := Open(Config{
		Enabled:   true,
		VaultKey:  raw,
		VaultPath: filepath.Join(t.TempDir(), "vault.json"),
	}); err != nil {
		t.Fatalf("raw-url base64 的 32 字节 key 应被接受: %v", err)
	}
}

func TestListBuiltins(t *testing.T) {
	m := newTestManager(t, nil)
	all := m.List()
	want := []string{"github", "google", "notion", "openai"}
	if len(all) != len(want) {
		t.Fatalf("内置连接器数量 %d，期望 %d", len(all), len(want))
	}
	for i, name := range want {
		if all[i].Name != name {
			t.Fatalf("第 %d 个连接器 %q，期望 %q", i, all[i].Name, name)
		}
		if all[i].DisplayName == "" || len(all[i].EnvNames) == 0 || all[i].DocsURL == "" {
			t.Fatalf("%s 的元信息不完整: %+v", name, all[i])
		}
	}

	byName := map[string]Connector{}
	for _, c := range all {
		byName[c.Name] = c
	}
	if got := byName["github"].Scopes; len(got) != 2 || got[0] != "repo" || got[1] != "read:user" {
		t.Fatalf("github scopes 不对: %v", got)
	}
	if got := byName["github"].EnvNames[0]; got != "GITHUB_TOKEN" {
		t.Fatalf("github env 名不对: %s", got)
	}
	if got := byName["google"].Scopes; len(got) != 1 || got[0] != "https://www.googleapis.com/auth/gmail.modify" {
		t.Fatalf("google scopes 不对: %v", got)
	}
	if got := byName["google"].EnvNames[0]; got != "GOOGLE_ACCESS_TOKEN" {
		t.Fatalf("google env 名不对: %s", got)
	}
	if byName["google"].Kind != KindOAuth2 || byName["notion"].Kind != KindOAuth2 {
		t.Fatal("google/notion 应为 oauth2")
	}
	if got := byName["notion"].EnvNames[0]; got != "NOTION_TOKEN" {
		t.Fatalf("notion env 名不对: %s", got)
	}
	openai := byName["openai"]
	if openai.Kind != KindAPIKey {
		t.Fatalf("openai 应为 apikey 型，实际 %v", openai.Kind)
	}
	if openai.EnvNames[0] != "OPENAI_API_KEY" {
		t.Fatalf("openai env 名不对: %s", openai.EnvNames[0])
	}
	if !strings.Contains(openai.DocsURL, "platform.openai.com/api-keys") {
		t.Fatalf("openai docs 应指到 api-keys 页，实际 %s", openai.DocsURL)
	}
}

func TestPutKeyThenListHasNoPlaintext(t *testing.T) {
	m := newTestManager(t, nil)
	const secret = "sk-proj-super-secret-value-0123456789"
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", secret); err != nil {
		t.Fatalf("PutKey: %v", err)
	}

	creds, err := m.ListCredentials(t.Context(), "t1", "u1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(creds) != 1 || creds[0].Name != "openai" || creds[0].Kind != KindAPIKey {
		t.Fatalf("列表内容不对: %+v", creds)
	}
	if creds[0].Token != "" || creds[0].Refresh != "" {
		t.Fatal("ListCredentials 不应返回 token 字段")
	}

	// 真正要守住的是「任何序列化结果里都没有明文」——HTTP 层直接 writeJSON 这个切片。
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("序列化后出现明文 token: %s", raw)
	}
	if strings.Contains(string(raw), `"token"`) || strings.Contains(string(raw), `"refresh"`) {
		t.Fatalf("序列化后出现了 token 字段名: %s", raw)
	}
}

func TestPutKeyRejectsOAuthConnectorAndUnknown(t *testing.T) {
	m := newTestManager(t, nil)
	if err := m.PutKey(t.Context(), "t1", "u1", "github", "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("对 OAuth 连接器贴 key 应返回 ErrUnsupported，实际 %v", err)
	}
	if err := m.PutKey(t.Context(), "t1", "u1", "linear", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知连接器应返回 ErrNotFound，实际 %v", err)
	}
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", "   "); err == nil {
		t.Fatal("空白 key 应被拒绝")
	}
}

func TestAuthorizeURLGithub(t *testing.T) {
	m := newTestManager(t, nil)
	got, err := m.AuthorizeURL(t.Context(), "t1", "u1", "github", "", "")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("解析授权地址: %v", err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("授权端点不对: %s", got)
	}
	q := u.Query()
	if q.Get("client_id") != "gh-id" || q.Get("response_type") != "code" {
		t.Fatalf("授权参数不对: %s", u.RawQuery)
	}
	if q.Get("scope") != "repo read:user" {
		t.Fatalf("scope 应以空格分隔: %q", q.Get("scope"))
	}
	// redirect_uri 未由 handler 传入时，必须能从 BaseURL 拼出来。
	if q.Get("redirect_uri") != "https://astra.example.com/v1/connectors/callback" {
		t.Fatalf("redirect_uri 兜底不对: %q", q.Get("redirect_uri"))
	}
	tenant, user, name, err := m.ParseState(q.Get("state"))
	if err != nil {
		t.Fatalf("AuthorizeURL 生成的 state 应可解析: %v", err)
	}
	if tenant != "t1" || user != "u1" || name != "github" {
		t.Fatalf("state 内容不对: %s/%s/%s", tenant, user, name)
	}
}

func TestAuthorizeURLRedirectOverride(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.BaseURL = "" })
	got, err := m.AuthorizeURL(t.Context(), "t1", "u1", "github", "https://gw.example.com/cb", "")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("redirect_uri") != "https://gw.example.com/cb" {
		t.Fatalf("显式 redirect 应优先: %q", u.Query().Get("redirect_uri"))
	}

	// BaseURL 和 override 都没有时不能瞎编一个地址，直接不传。
	got2, err := m.AuthorizeURL(t.Context(), "t1", "u1", "github", "", "")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u2, _ := url.Parse(got2)
	if u2.Query().Has("redirect_uri") {
		t.Fatalf("没有可用回调地址时不应带 redirect_uri: %q", u2.Query().Get("redirect_uri"))
	}
}

func TestAuthorizeURLNotionAndGoogle(t *testing.T) {
	m := newTestManager(t, nil)

	notion, err := m.AuthorizeURL(t.Context(), "t1", "u1", "notion", "", "")
	if err != nil {
		t.Fatalf("notion AuthorizeURL: %v", err)
	}
	nu, _ := url.Parse(notion)
	if nu.Host != "api.notion.com" || nu.Path != "/v1/oauth/authorize" {
		t.Fatalf("notion 授权端点不对: %s", notion)
	}
	if nu.Query().Get("owner") != "user" {
		t.Fatalf("notion 必须带 owner=user: %s", nu.RawQuery)
	}
	if nu.Query().Has("scope") {
		t.Fatalf("notion 不接受 scope 参数，不该带: %s", nu.RawQuery)
	}

	google, err := m.AuthorizeURL(t.Context(), "t1", "u1", "google", "", "")
	if err != nil {
		t.Fatalf("google AuthorizeURL: %v", err)
	}
	gu, _ := url.Parse(google)
	if gu.Host != "accounts.google.com" || gu.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("google 授权端点不对: %s", google)
	}
	if gu.Query().Get("access_type") != "offline" {
		t.Fatal("google 需要 access_type=offline 才会返回 refresh_token")
	}
	if gu.Query().Get("scope") != "https://www.googleapis.com/auth/gmail.modify" {
		t.Fatalf("google scope 不对: %q", gu.Query().Get("scope"))
	}
}

func TestAuthorizeURLAPIKeyAndUnsupported(t *testing.T) {
	m := newTestManager(t, nil)
	got, err := m.AuthorizeURL(t.Context(), "t1", "u1", "openai", "", "")
	if err != nil {
		t.Fatalf("apikey 型不该报错: %v", err)
	}
	if got != "" {
		t.Fatalf("apikey 型应返回空串（上层据此回 needs_key），实际 %q", got)
	}
	if _, err := m.AuthorizeURL(t.Context(), "t1", "u1", "linear", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知连接器应返回 ErrNotFound，实际 %v", err)
	}

	bare := newTestManager(t, func(c *Config) {
		c.GitHubClientID, c.GitHubClientSecret = "", ""
	})
	if _, err := bare.AuthorizeURL(t.Context(), "t1", "u1", "github", "", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("没配 OAuth 应用凭据应返回 ErrUnsupported，实际 %v", err)
	}
	if _, err := m.AuthorizeURL(t.Context(), "t1", "u1", "github", "", "junk-state"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("调用方塞了坏 state 应被拒绝，实际 %v", err)
	}
}

func TestExchangeStoresCredential(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单: %v", err)
		}
		gotForm = r.PostForm
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("github 应走 form 编码，实际 %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_new","refresh_token":"ghr_new","expires_in":3600,"scope":"repo, read:user","token_type":"bearer"}`))
	}))
	defer srv.Close()

	m := newTestManager(t, nil)
	m.tokenURLs["github"] = srv.URL

	cred, err := m.Exchange(t.Context(), "t1", "u1", "github", "the-code", "")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if cred.Name != "github" || cred.Kind != KindOAuth2 || cred.LinkedAt.IsZero() {
		t.Fatalf("返回的凭据元信息不对: %+v", cred)
	}
	if cred.Token != "" || cred.Refresh != "" {
		t.Fatal("Exchange 不该把明文 token 返回给调用方")
	}
	if gotForm.Get("code") != "the-code" || gotForm.Get("client_id") != "gh-id" ||
		gotForm.Get("client_secret") != "gh-secret" ||
		gotForm.Get("redirect_uri") != "https://astra.example.com/v1/connectors/callback" {
		t.Fatalf("换 token 的请求参数不对: %v", gotForm)
	}
	if gotForm.Has("grant_type") {
		t.Fatalf("github 的 web flow 不该带 grant_type: %v", gotForm)
	}

	stored, err := m.vault.get("t1", "u1", "github")
	if err != nil {
		t.Fatalf("vault.get: %v", err)
	}
	if stored.Token != "gho_new" || stored.Refresh != "ghr_new" {
		t.Fatalf("落库的 token 不对: %+v", stored)
	}
	if len(stored.Scopes) != 2 {
		t.Fatalf("scopes 解析不对（github 用逗号分隔）: %v", stored.Scopes)
	}
	// expires_in=3600 必须变成绝对过期时间，且落在合理区间。
	if d := time.Until(stored.Expires); d < 50*time.Minute || d > 61*time.Minute {
		t.Fatalf("过期时间换算不对: %v", d)
	}
}

func TestExchangeNotionUsesBasicAuthJSON(t *testing.T) {
	var (
		authOK bool
		body   map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("notion 应走 JSON body，实际 %q", ct)
		}
		id, secret, ok := r.BasicAuth()
		authOK = ok && id == "no-id" && secret == "no-secret"
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"access_token":"secret_notion","workspace_name":"ws","bot_id":"b1"}`))
	}))
	defer srv.Close()

	m := newTestManager(t, nil)
	m.tokenURLs["notion"] = srv.URL
	if _, err := m.Exchange(t.Context(), "t1", "u1", "notion", "code-1", ""); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !authOK {
		t.Fatal("notion 换 token 必须用 HTTP Basic（client_id:client_secret）")
	}
	if body["grant_type"] != "authorization_code" || body["code"] != "code-1" {
		t.Fatalf("notion 请求体不对: %v", body)
	}
	if _, ok := body["client_secret"]; ok {
		t.Fatal("notion 的 client_secret 不该出现在 body 里")
	}
	stored, err := m.vault.get("t1", "u1", "notion")
	if err != nil || stored.Token != "secret_notion" {
		t.Fatalf("notion 凭据落库不对: %+v err=%v", stored, err)
	}
}

func TestExchangeErrors(t *testing.T) {
	m := newTestManager(t, nil)

	if _, err := m.Exchange(t.Context(), "t1", "u1", "openai", "code", ""); err == nil {
		t.Fatal("apikey 型不该能走 Exchange")
	}
	if _, err := m.Exchange(t.Context(), "t1", "u1", "github", "", ""); err == nil {
		t.Fatal("空 code 应报错")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
	}))
	defer bad.Close()
	m.tokenURLs["github"] = bad.URL
	if _, err := m.Exchange(t.Context(), "t1", "u1", "github", "code", ""); err == nil {
		t.Fatal("提供方返回 400 时 Exchange 必须失败")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token_type":"bearer"}`))
	}))
	defer empty.Close()
	m.tokenURLs["github"] = empty.URL
	if _, err := m.Exchange(t.Context(), "t1", "u1", "github", "code", ""); err == nil {
		t.Fatal("响应里没有 access_token 时应报错")
	}
}

func TestInjectProxyIsHonest(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.InjectMode = InjectProxy })
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", "sk-x"); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	env, err := m.Inject(t.Context(), "t1", "u1", []string{"openai"})
	if !errors.Is(err, ErrInjectProxyUnimplemented) {
		t.Fatalf("proxy 模式必须如实报未实现，实际 %v", err)
	}
	if env == nil || len(env) != 0 {
		t.Fatalf("proxy 模式应返回空 map，实际 %v", env)
	}
	if strings.Contains(err.Error(), "未实现") == false {
		t.Fatalf("错误信息应说明未实现: %v", err)
	}
}

func TestInjectDefaultModeIsProxy(t *testing.T) {
	m := newTestManager(t, nil) // InjectMode 为空 → 默认 proxy
	if _, err := m.Inject(t.Context(), "t1", "u1", []string{"openai"}); !errors.Is(err, ErrInjectProxyUnimplemented) {
		t.Fatalf("默认应走 proxy（不会悄悄降级成 env）: %v", err)
	}
}

func TestInjectEnv(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.InjectMode = InjectEnv })
	const secret = "sk-live-abc"
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", secret); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	env, err := m.Inject(t.Context(), "t1", "u1", []string{"openai"})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if env["OPENAI_API_KEY"] != secret {
		t.Fatalf("注入的环境变量不对: %v", env)
	}
	if len(env) != 1 {
		t.Fatalf("只应注入一个变量: %v", env)
	}

	// 空名单：什么都不做，也不该报错。
	got, err := m.Inject(t.Context(), "t1", "u1", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("空名单应返回空 map: %v %v", got, err)
	}

	// 没连接过
	if _, err := m.Inject(t.Context(), "t1", "u1", []string{"github"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("未连接应返回 ErrNotConnected，实际 %v", err)
	}
	// 只属于 t1/u1：别的租户拿不到。
	if _, err := m.Inject(t.Context(), "t2", "u1", []string{"openai"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("跨租户应返回 ErrNotConnected，实际 %v", err)
	}
}

func TestInjectRefreshesExpiredToken(t *testing.T) {
	var refreshCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "old-refresh" {
			t.Errorf("刷新请求参数不对: %v", r.PostForm)
		}
		refreshCalled = true
		_, _ = w.Write([]byte(`{"access_token":"gho_refreshed","expires_in":3600,"scope":"repo"}`))
	}))
	defer srv.Close()

	m := newTestManager(t, func(c *Config) { c.InjectMode = InjectEnv })
	m.tokenURLs["github"] = srv.URL

	// 一条已经过期的凭据（带 refresh_token）。
	expired := Credential{
		TenantID: "t1", UserID: "u1", Name: "github", Kind: KindOAuth2,
		Token:    "gho_old",
		Refresh:  "old-refresh",
		Expires:  time.Now().Add(-time.Minute),
		Scopes:   []string{"repo"},
		LinkedAt: time.Now().Add(-2 * time.Hour),
	}
	if err := m.vault.set(expired); err != nil {
		t.Fatalf("vault.set: %v", err)
	}

	env, err := m.Inject(t.Context(), "t1", "u1", []string{"github"})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !refreshCalled {
		t.Fatal("过期 token 应先刷新再注入")
	}
	if env["GITHUB_TOKEN"] != "gho_refreshed" {
		t.Fatalf("注入的应是刷新后的 token: %v", env)
	}
	// 刷新结果必须落库，否则每次注入都会再刷一次。
	stored, err := m.vault.get("t1", "u1", "github")
	if err != nil || stored.Token != "gho_refreshed" {
		t.Fatalf("刷新后未落库: %+v err=%v", stored, err)
	}
	if stored.Refresh != "old-refresh" {
		t.Fatalf("提供方没回新 refresh_token 时应沿用旧的，实际 %q", stored.Refresh)
	}
	if !stored.LinkedAt.Equal(expired.LinkedAt) {
		t.Fatalf("刷新不该改 linked_at: %v", stored.LinkedAt)
	}
}

func TestInjectKeepsTokenWhenNotRefreshable(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.InjectMode = InjectEnv })
	// 过期但没有 refresh_token：把原 token 交出去，由上游 API 自己 401。
	if err := m.vault.set(Credential{
		TenantID: "t1", UserID: "u1", Name: "openai", Kind: KindAPIKey,
		Token: "sk-old", LinkedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("vault.set: %v", err)
	}
	env, err := m.Inject(t.Context(), "t1", "u1", []string{"openai"})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if env["OPENAI_API_KEY"] != "sk-old" {
		t.Fatalf("apikey 不该被刷新逻辑影响: %v", env)
	}
}

func TestDeleteAndListAcrossTenants(t *testing.T) {
	m := newTestManager(t, nil)
	if err := m.PutKey(t.Context(), "t1", "u1", "openai", "k1"); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	if err := m.PutKey(t.Context(), "t2", "u1", "openai", "k2"); err != nil {
		t.Fatalf("PutKey: %v", err)
	}
	creds, err := m.ListCredentials(t.Context(), "t1", "u1")
	if err != nil || len(creds) != 1 {
		t.Fatalf("租户隔离失败: %v %v", creds, err)
	}
	if err := m.Delete(t.Context(), "t1", "u1", "openai"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := m.Delete(t.Context(), "t1", "u1", "openai"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := m.ListCredentials(t.Context(), "t2", "u1"); err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	// 关掉再打开（模拟重启）后凭据仍在。
	path := m.cfg.VaultPath
	reopened, err := Open(Config{Enabled: true, VaultKey: m.cfg.VaultKey, VaultPath: path})
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	creds, err = reopened.ListCredentials(t.Context(), "t2", "u1")
	if err != nil || len(creds) != 1 {
		t.Fatalf("重启后凭据丢失: %v %v", creds, err)
	}
}
