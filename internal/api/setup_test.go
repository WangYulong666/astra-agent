package api

// 首启向导与 LLM 设置的单测。
// 复用 platform_test.go 的 fakeAdmin/fakeAuth，但路由要同时注册
// registerPlatform 与 registerSetup（向导端点在后者里）。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeLLMSettings 记录 Set 的调用，Current 可被测试手动拨动。
type fakeLLMSettings struct {
	mu    sync.Mutex
	key   string
	base  string
	sets  [][2]string
	setFn func(key, base string) error
}

func (f *fakeLLMSettings) Current() (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key, f.base, true
}

func (f *fakeLLMSettings) Set(key, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setFn != nil {
		if err := f.setFn(key, base); err != nil {
			return err
		}
	}
	f.sets = append(f.sets, [2]string{key, base})
	f.key, f.base = key, base
	return nil
}

func newSetupHarness(t *testing.T, llm LLMSettingsStore) *platformHarness {
	t.Helper()
	db := &fakeAdmin{}
	h := &handler{
		cfg: Config{Auth: &fakeAuth{db: db}, Admin: db, RequireAuth: true, LLMSettings: llm},
		log: log.New(io.Discard, "", 0),
	}
	mux := http.NewServeMux()
	h.registerPlatform(mux)
	h.registerSetup(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &platformHarness{srv: srv, db: db}
}

func TestSetupWizardFlow(t *testing.T) {
	h := newSetupHarness(t, nil)

	// 1. 空系统：needsSetup=true
	code, body := h.do(t, "GET", "/v1/setup/state", "", nil)
	if code != 200 {
		t.Fatalf("setup/state = %d", code)
	}
	var st struct {
		NeedsSetup bool `json:"needsSetup"`
	}
	_ = json.Unmarshal(body, &st)
	if !st.NeedsSetup {
		t.Fatal("空系统 needsSetup 应为 true")
	}

	// 2. complete 建租户+管理员，直接发 token
	code, body = h.do(t, "POST", "/v1/setup/complete", "", map[string]any{
		"email": "boss@example.com", "password": "supersecret1", "slug": "acme",
	})
	if code != 201 {
		t.Fatalf("setup/complete = %d: %s", code, body)
	}
	var done struct {
		AutoLogin bool   `json:"autoLogin"`
		Token     string `json:"token"`
	}
	_ = json.Unmarshal(body, &done)
	if !done.AutoLogin || done.Token == "" {
		t.Fatalf("应自动登录并返回 token: %s", body)
	}

	// 3. 已初始化后再次调用 → 409
	code, _ = h.do(t, "POST", "/v1/setup/complete", "", map[string]any{
		"email": "x@y.com", "password": "supersecret1",
	})
	if code != 409 {
		t.Fatalf("第二次 complete 应 409，得到 %d", code)
	}

	// 4. state 翻转为 false
	_, body = h.do(t, "GET", "/v1/setup/state", "", nil)
	_ = json.Unmarshal(body, &st)
	if st.NeedsSetup {
		t.Fatal("初始化后 needsSetup 应为 false")
	}

	// 5. 拿向导发的 token 真的能访问治理接口
	code, _ = h.do(t, "GET", "/v1/admin/llm-settings", done.Token, nil)
	if code != 200 {
		t.Fatalf("向导 token 应可用，llm-settings = %d", code)
	}
}

func TestSetupCompleteValidation(t *testing.T) {
	h := newSetupHarness(t, nil)
	// 密码太短
	if code, _ := h.do(t, "POST", "/v1/setup/complete", "", map[string]any{
		"email": "a@b.c", "password": "short",
	}); code != 400 {
		t.Fatalf("短密码应 400，得到 %d", code)
	}
	// 缺邮箱
	if code, _ := h.do(t, "POST", "/v1/setup/complete", "", map[string]any{
		"password": "supersecret1",
	}); code != 400 {
		t.Fatalf("缺邮箱应 400，得到 %d", code)
	}
}

func TestLLMSettingsRoundTrip(t *testing.T) {
	llm := &fakeLLMSettings{}
	h := newSetupHarness(t, llm)
	// 写全局上游要求平台管理员；这里把 owner 放进白名单，
	// 「不在白名单 → 403」由 TestLLMSettingsRequiresPlatformAdmin 单独覆盖。
	t.Setenv(platformAdminEnv, "owner@acme.test")

	// 未初始化的系统没有身份，管理端点必须拒绝
	if code, _ := h.do(t, "GET", "/v1/admin/llm-settings", "", nil); code == 200 {
		t.Fatal("无 token 读 llm-settings 不应 200")
	}

	// 建个 owner（直接种数据，绕过向导——这测的是设置端点不是向导）
	_, owner, _, _ := h.seedTenant(t)

	// GET 不回 key 本体
	code, body := h.do(t, "GET", "/v1/admin/llm-settings", owner, nil)
	if code != 200 {
		t.Fatalf("GET llm-settings = %d", code)
	}
	if string(body) == "" || jsonContains(body, "sk-secret") {
		t.Fatalf("响应不得包含 key 本体: %s", body)
	}

	// PUT 热更 → Current 生效
	code, body = h.do(t, "POST", "/v1/admin/llm-settings", owner, map[string]any{
		"apiKey": "sk-secret", "baseUrl": "https://api.example.com",
	})
	if code != 200 {
		t.Fatalf("PUT llm-settings = %d: %s", code, body)
	}
	key, base, _ := llm.Current()
	if key != "sk-secret" || base != "https://api.example.com" {
		t.Fatalf("热更未生效: %q %q", key, base)
	}
}

func jsonContains(b []byte, sub string) bool {
	return len(sub) > 0 && string(b) != "" && indexOf(string(b), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestLLMSettingsProbe(t *testing.T) {
	// probeUpstream 对错误端口要报错而不是挂死
	if _, err := probeUpstream(context.Background(), "http://127.0.0.1:1", "sk-x"); err == nil {
		t.Fatal("对不可达上游应报错")
	}
}

func TestLLMSettingsKeepKeyAndClear(t *testing.T) {
	llm := &fakeLLMSettings{}
	h := newSetupHarness(t, llm)
	t.Setenv(platformAdminEnv, "owner@acme.test")
	_, owner, _, _ := h.seedTenant(t)

	// 先存一把 key
	if code, _ := h.do(t, "POST", "/v1/admin/llm-settings", owner, map[string]any{
		"apiKey": "sk-keep-me", "baseUrl": "https://a.example.com",
	}); code != 200 {
		t.Fatalf("初始保存 = %d", code)
	}

	// keepKey=true：只改 baseUrl，key 保留
	if code, body := h.do(t, "POST", "/v1/admin/llm-settings", owner, map[string]any{
		"keepKey": true, "baseUrl": "https://b.example.com",
	}); code != 200 {
		t.Fatalf("keepKey 保存 = %d: %s", code, body)
	}
	key, base, _ := llm.Current()
	if key != "sk-keep-me" || base != "https://b.example.com" {
		t.Fatalf("keepKey 未生效: key=%q base=%q", key, base)
	}

	// keepKey=true 且带 apiKey → 400
	if code, _ := h.do(t, "POST", "/v1/admin/llm-settings", owner, map[string]any{
		"keepKey": true, "apiKey": "sk-x",
	}); code != 400 {
		t.Fatalf("keepKey+apiKey 应 400，得到 %d", code)
	}

	// clear=true：显式清除
	if code, _ := h.do(t, "POST", "/v1/admin/llm-settings", owner, map[string]any{"clear": true}); code != 200 {
		t.Fatal("clear 保存失败")
	}
	key, _, _ = llm.Current()
	if key != "" {
		t.Fatalf("clear 未清掉 key: %q", key)
	}
}
