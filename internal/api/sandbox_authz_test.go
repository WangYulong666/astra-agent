package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/auth"
)

// 沙箱 key 的白名单就是它的全部权限边界（authed 里沙箱分支不再过 RBAC），
// 所以这条用例必须锁死：多放开一个前缀就是横向越权。
func TestSandboxPathAllowed(t *testing.T) {
	h := &handler{}
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/meta", true},
		{"GET", "/v1/capabilities", true},
		{"GET", "/v1/capabilities?detail=full", true},
		{"GET", "/v1/capabilities/memory_ingest", true},
		{"GET", "/v1/skills", true},
		{"GET", "/v1/skills/astra-run", true},
		{"GET", "/v1/memory/search?q=x", true},
		{"GET", "/v1/memory/stats", true},
		// 明确不该放开的
		{"GET", "/v1/sessions", false},
		{"GET", "/v1/sessions/abc", false},
		{"GET", "/v1/runs/abc/events", false},
		{"GET", "/v1/admin/audit", false},
		{"GET", "/v1/admin/egress/log", false},
		{"GET", "/v1/insights", false},
		{"GET", "/v1/connectors", false},
		{"POST", "/v1/memory/ingest", false},
		{"DELETE", "/v1/memory/abc", false},
		{"GET", "/v1/skills/drafts", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		got := h.sandboxPathAllowed(r)
		if got != c.want {
			t.Errorf("%s %s = %v，期望 %v", c.method, c.path, got, c.want)
		}
	}
	_ = http.MethodGet
}

// 端到端串起「签发 → 识别 → 白名单」：T4 的 auth 单测只覆盖到 Verify，
// 这里补上 api 侧的实际分支，防止 authed 里的接线被改坏。
func TestTrySandboxAuthEndToEnd(t *testing.T) {
	// 复用同包 sse_ticket_test.go 的 fakeAdmin：*store.File 不实现 AdminStore，
	// 而 trySandboxAuth 只需要一个能验签的 Service，不需要真治理库。
	secret := []byte("loop-verify-secret-key-32-bytes-long")
	svc, err := auth.New(auth.Config{Store: &fakeAdmin{}, Secret: secret})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := &handler{cfg: Config{Auth: svc}}

	tok, _, err := svc.IssueSandboxKey("tenant-1", "user-1", "session-1", 0)
	if err != nil {
		t.Fatalf("签发: %v", err)
	}

	check := func(path, token string) (isSandbox, valid bool, sid string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		p, is, ok := h.trySandboxAuth(r)
		if ok && (p.TenantID != "tenant-1" || p.UserID != "user-1") {
			t.Errorf("%s: principal 归属不对: %+v", path, p)
		}
		return is, ok, sandboxSessionID(r)
	}

	if is, ok, sid := check("/v1/capabilities", tok); !is || !ok || sid != "session-1" {
		t.Fatalf("白名单内应放行且注入 sessionID，实际 is=%v ok=%v sid=%q", is, ok, sid)
	}
	if is, ok, _ := check("/v1/admin/audit", tok); !is || ok {
		t.Fatalf("白名单外必须拒绝，实际 is=%v ok=%v", is, ok)
	}
	if is, _, _ := check("/v1/capabilities", "garbage-token"); is {
		t.Fatal("坏 token 不能算沙箱凭据，应交回主鉴权链 401")
	}
	if is, _, _ := check("/v1/capabilities", ""); is {
		t.Fatal("无 token 不能算沙箱凭据")
	}
}
