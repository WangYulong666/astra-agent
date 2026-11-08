package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

const sandboxTestSecret = "loop-verify-secret-key-32-bytes-long"

// newSandboxFixture 造一个「能验签 + 有真实会话存储」的 handler。
// trySandboxAuth 现在要用 h.cfg.Store.GetSession 复核会话是否还存在，
// 所以夹具必须显式传一个真实 store.OpenFile，不能留 nil 走兼容捷径。
// auth.New 另需一个 store.AdminStore（*store.File 不实现它），沿用同包的 fakeAdmin。
func newSandboxFixture(t *testing.T) (*handler, *auth.Service, store.Store) {
	t.Helper()
	st, err := store.OpenFile(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("store.OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := auth.New(auth.Config{Store: &fakeAdmin{}, Secret: []byte(sandboxTestSecret)})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return &handler{cfg: Config{Auth: svc, Store: st}}, svc, st
}

func mkSandboxSession(t *testing.T, st store.Store, id string) {
	t.Helper()
	if err := st.CreateSession(context.Background(), store.Session{
		ID: id, TenantID: "tenant-1", UserID: "user-1", Status: store.SessionActive,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
}

// sandboxCheck 直接调 trySandboxAuth（不经过 authed），返回注入的 sessionID。
func sandboxCheck(h *handler, path, token string) (isSandbox, valid bool, sid string, p auth.Principal) {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	pr, is, ok := h.trySandboxAuth(r)
	return is, ok, sandboxSessionID(r), pr
}

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
		{"GET", "/v1/skills/drafts/abc/accept", false},
		{"GET", "/v1/capabilities/foo/bar", false},
		{"POST", "/v1/meta", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		got := h.sandboxPathAllowed(r)
		if got != c.want {
			t.Errorf("%s %s = %v，期望 %v", c.method, c.path, got, c.want)
		}
	}
}

// 端到端串起「签发 → 识别 → 会话存在性 → 白名单」：T4 的 auth 单测只覆盖到
// Verify，这里补上 api 侧的实际分支，防止 authed 里的接线被改坏。
func TestTrySandboxAuthEndToEnd(t *testing.T) {
	h, svc, st := newSandboxFixture(t)
	// 两个会话都存在：验证 key 只注入自己那个 sessionID，拿不到别人的。
	mkSandboxSession(t, st, "session-1")
	mkSandboxSession(t, st, "session-2")

	tok1, _, err := svc.IssueSandboxKey("tenant-1", "user-1", "session-1", 0)
	if err != nil {
		t.Fatalf("签发 session-1: %v", err)
	}
	tok2, _, err := svc.IssueSandboxKey("tenant-1", "user-1", "session-2", 0)
	if err != nil {
		t.Fatalf("签发 session-2: %v", err)
	}

	// 白名单内放行、归属正确、注入的 sessionID 是该 key 自己的会话。
	if is, ok, sid, p := sandboxCheck(h, "/v1/capabilities", tok1); !is || !ok || sid != "session-1" ||
		p.TenantID != "tenant-1" || p.UserID != "user-1" || p.Via != "sandbox" {
		t.Fatalf("白名单内应放行且注入自己的 sessionID，实际 is=%v ok=%v sid=%q p=%+v", is, ok, sid, p)
	}
	// 跨会话：session-2 的 key 注入的是 session-2，绝不会因为 session-1 也存在就串过去。
	if is, ok, sid, _ := sandboxCheck(h, "/v1/capabilities", tok2); !is || !ok || sid != "session-2" {
		t.Fatalf("跨会话 key 必须注入自己的 sessionID，实际 is=%v ok=%v sid=%q", is, ok, sid)
	}
	// 反过来，用 session-1 的 key 也访问不到别的会话的路径（白名单直接拒绝）。
	if is, ok, _, _ := sandboxCheck(h, "/v1/sessions/session-2", tok1); !is || ok {
		t.Fatalf("sessions 路径未放开，必须拒绝，实际 is=%v ok=%v", is, ok)
	}
	if is, ok, _, _ := sandboxCheck(h, "/v1/admin/audit", tok1); !is || ok {
		t.Fatalf("白名单外必须拒绝，实际 is=%v ok=%v", is, ok)
	}
	if is, _, _, _ := sandboxCheck(h, "/v1/capabilities", "garbage-token"); is {
		t.Fatal("坏 token 不能算沙箱凭据，应交回主鉴权链 401")
	}
	if is, _, _, _ := sandboxCheck(h, "/v1/capabilities", ""); is {
		t.Fatal("无 token 不能算沙箱凭据")
	}
}

// 会话存在性校验 = 吊销。逐条覆盖删除会话的两种落地形态，以及 Store 为 nil
// 的 fail-closed，确保没有「静默放行」的口子。
func TestTrySandboxAuthSessionRevocation(t *testing.T) {
	ctx := context.Background()

	t.Run("会话存在→放行", func(t *testing.T) {
		h, svc, st := newSandboxFixture(t)
		mkSandboxSession(t, st, "live")
		tok, _, _ := svc.IssueSandboxKey("tenant-1", "user-1", "live", 0)
		if is, ok, sid, _ := sandboxCheck(h, "/v1/capabilities", tok); !is || !ok || sid != "live" {
			t.Fatalf("会话存在应放行，实际 is=%v ok=%v sid=%q", is, ok, sid)
		}
	})

	t.Run("会话软删标记 SessionDeleted→拒绝", func(t *testing.T) {
		h, svc, st := newSandboxFixture(t)
		mkSandboxSession(t, st, "soft-deleted")
		tok, _, _ := svc.IssueSandboxKey("tenant-1", "user-1", "soft-deleted", 0)
		// 复刻 runner.DeleteSession 的第一步：先标 deleted，再删目录。
		sess, err := st.GetSession(ctx, "soft-deleted")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		sess.Status = store.SessionDeleted
		if err := st.PutSession(ctx, sess); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
		if is, ok, _, _ := sandboxCheck(h, "/v1/capabilities", tok); !is || ok {
			t.Fatalf("会话已标记删除，key 必须失效，实际 is=%v ok=%v", is, ok)
		}
	})

	t.Run("会话目录已删→拒绝", func(t *testing.T) {
		h, svc, st := newSandboxFixture(t)
		mkSandboxSession(t, st, "hard-deleted")
		tok, _, _ := svc.IssueSandboxKey("tenant-1", "user-1", "hard-deleted", 0)
		if err := st.DeleteSession(ctx, "hard-deleted"); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if is, ok, _, _ := sandboxCheck(h, "/v1/capabilities", tok); !is || ok {
			t.Fatalf("会话目录已删，key 必须失效，实际 is=%v ok=%v", is, ok)
		}
	})

	t.Run("会话从未存在→拒绝", func(t *testing.T) {
		h, svc, _ := newSandboxFixture(t)
		tok, _, _ := svc.IssueSandboxKey("tenant-1", "user-1", "ghost-9", 0)
		if is, ok, _, _ := sandboxCheck(h, "/v1/capabilities", tok); !is || ok {
			t.Fatalf("会话不存在，key 必须失效，实际 is=%v ok=%v", is, ok)
		}
	})

	t.Run("Store 为 nil→fail-closed 拒绝", func(t *testing.T) {
		_, svc, st := newSandboxFixture(t)
		mkSandboxSession(t, st, "nil-store")
		tok, _, _ := svc.IssueSandboxKey("tenant-1", "user-1", "nil-store", 0)
		// 故意不挂 Store：验签通过但无法证明会话仍存在，必须拒绝，不能静默放行。
		hnil := &handler{cfg: Config{Auth: svc}}
		if is, ok, _, _ := sandboxCheck(hnil, "/v1/capabilities", tok); !is || ok {
			t.Fatalf("Store 为 nil 必须 fail-closed，实际 is=%v ok=%v", is, ok)
		}
	})
}
