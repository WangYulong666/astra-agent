package api

// 多租户旁路越权的回归测试。
//
// 覆盖四条 P0：insights 全量诊断、egress 的 ?tenant= 跨租户、全局 LLM 上游
// 被租户 owner 改写、obs/traces 返回全项目 trace。都用「构造两个租户的身份
// + 直调 handler」的方式，不依赖沙箱进程。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/insight"
	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/WangYulong666/astra-agent/internal/trace"
)

// withPrincipal 把身份注入请求上下文，等价于 authed 中间件做过的事。
func withPrincipal(r *http.Request, p auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

func newIsolationHandler(t *testing.T) (*handler, store.Store) {
	t.Helper()
	st, err := store.OpenFile(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("store.OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &handler{
		cfg: Config{Store: st},
		log: log.New(io.Discard, "", 0),
	}, st
}

// seedTenantWorld 造两个租户，各一个会话、一个 run。
func seedTenantWorld(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, x := range []struct{ tid, sid, rid string }{
		{"t1", "s-a", "r-a"},
		{"t2", "s-b", "r-b"},
	} {
		if err := st.CreateSession(ctx, store.Session{
			ID: x.sid, TenantID: x.tid, UserID: "u-" + x.tid,
			Status: store.SessionActive, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", x.sid, err)
		}
		if err := st.CreateRun(ctx, store.Run{
			ID: x.rid, SessionID: x.sid, Status: store.RunFailed,
			CreatedAt: time.Now().UTC(), NextSeq: 1,
		}); err != nil {
			t.Fatalf("CreateRun(%s): %v", x.rid, err)
		}
	}
}

// ── 1. /v1/insights ─────────────────────────────────────────

// seedInsights 直接写 insights.json：Store 接口没有「写入诊断」的公开方法
// （生产路径只有 Attach 会写），这里按落盘格式造数据。
func seedInsights(t *testing.T, dir string, items []insight.Diagnosis) insight.Store {
	t.Helper()
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "insights.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	ins, err := insight.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ins
}

func listInsightsAs(t *testing.T, h *handler, p auth.Principal, query string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/insights"+query, nil)
	if p.Via != "" || p.UserID != "" || p.TenantID != "" {
		r = withPrincipal(r, p)
	}
	w := httptest.NewRecorder()
	h.listInsights(w, r)
	return w.Code, w.Body.Bytes()
}

func decodeInsightRunIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Items []insight.Diagnosis `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析 items 失败: %v: %s", err, body)
	}
	out := make([]string, 0, len(resp.Items))
	for _, d := range resp.Items {
		out = append(out, d.RunID)
	}
	return out
}

// TestInsightsTenantIsolation 是本组最重要的用例：不带 session_id 时
// 只能看到自己租户的诊断；别的租户的记录与无归属老数据都不能出现。
func TestInsightsTenantIsolation(t *testing.T) {
	h, st := newIsolationHandler(t)
	seedTenantWorld(t, st)
	h.cfg.Insights = seedInsights(t, t.TempDir(), []insight.Diagnosis{
		{RunID: "r-a", SessionID: "s-a", TenantID: "t1", UserID: "u-t1", Error: "a", CreatedAt: time.Now().UTC()},
		{RunID: "r-b", SessionID: "s-b", TenantID: "t2", UserID: "u-t2", Error: "b", CreatedAt: time.Now().UTC()},
		// 接入认证之前写入的老数据：没有归属。
		{RunID: "r-old", SessionID: "s-legacy", Error: "old", CreatedAt: time.Now().UTC()},
	})

	code, body := listInsightsAs(t, h, auth.Principal{
		UserID: "u-t1", TenantID: "t1", Email: "a@t1.test", Role: store.RoleAdmin, Via: auth.ViaJWT,
	}, "")
	if code != http.StatusOK {
		t.Fatalf("t1 列表返回 %d: %s", code, body)
	}
	ids := decodeInsightRunIDs(t, body)
	if len(ids) != 1 || ids[0] != "r-a" {
		t.Fatalf("t1 看到了不该看的诊断: %v（应只有 r-a，r-b 与无归属老数据都不可见）", ids)
	}

	code, body = listInsightsAs(t, h, auth.Principal{
		UserID: "u-t2", TenantID: "t2", Email: "b@t2.test", Role: store.RoleAdmin, Via: auth.ViaJWT,
	}, "")
	if code != http.StatusOK {
		t.Fatalf("t2 列表返回 %d: %s", code, body)
	}
	if ids := decodeInsightRunIDs(t, body); len(ids) != 1 || ids[0] != "r-b" {
		t.Fatalf("t2 看到了不该看的诊断: %v", ids)
	}

	// 单机兼容模式：没有租户概念，全量（含老数据）就是「自己的全部」。
	code, body = listInsightsAs(t, h, auth.Principal{Via: auth.ViaDev}, "")
	if code != http.StatusOK {
		t.Fatalf("dev 列表返回 %d: %s", code, body)
	}
	if ids := decodeInsightRunIDs(t, body); len(ids) != 3 {
		t.Fatalf("dev 模式应看到全部 3 条（含无归属老数据），got %v", ids)
	}
}

// TestInsightsSessionIDOwnershipIs404：?session_id= 别人的会话必须 404，
// 403 等于告诉对方「这个会话存在」。
func TestInsightsSessionIDOwnershipIs404(t *testing.T) {
	h, st := newIsolationHandler(t)
	seedTenantWorld(t, st)
	h.cfg.Insights = seedInsights(t, t.TempDir(), []insight.Diagnosis{
		{RunID: "r-b", SessionID: "s-b", TenantID: "t2", UserID: "u-t2", Error: "b", CreatedAt: time.Now().UTC()},
	})
	p1 := auth.Principal{UserID: "u-t1", TenantID: "t1", Role: store.RoleAdmin, Via: auth.ViaJWT}

	if code, body := listInsightsAs(t, h, p1, "?session_id=s-b"); code != http.StatusNotFound {
		t.Fatalf("跨租户 session_id 返回 %d，期望 404: %s", code, body)
	}
	// 不存在的会话同样是 404，两种失败无法区分。
	if code, _ := listInsightsAs(t, h, p1, "?session_id=ghost"); code != http.StatusNotFound {
		t.Fatalf("不存在的 session_id 返回 %d，期望 404", code)
	}
	// 自己的会话可用。
	if code, body := listInsightsAs(t, h, p1, "?session_id=s-a"); code != http.StatusOK {
		t.Fatalf("自己的 session_id 返回 %d: %s", code, body)
	}
	// 同租户的普通成员不能看别人的会话。
	if code, _ := listInsightsAs(t, h, auth.Principal{
		UserID: "u-other", TenantID: "t1", Role: store.RoleMember, Via: auth.ViaJWT,
	}, "?session_id=s-a"); code != http.StatusNotFound {
		t.Fatalf("member 读他人会话返回 %d，期望 404", code)
	}
}

// ── 2. egress ?tenant= ──────────────────────────────────────

type fakeEgress struct {
	mu      sync.Mutex
	queried []string
	set     []string
}

func (f *fakeEgress) Enabled() bool { return true }
func (f *fakeEgress) Addr() string  { return "127.0.0.1:0" }
func (f *fakeEgress) Policy(_ context.Context, tenantID string) (egress.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, tenantID)
	return egress.Policy{Default: egress.Deny}, nil
}
func (f *fakeEgress) SetPolicy(_ context.Context, tenantID string, _ egress.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = append(f.set, tenantID)
	return nil
}
func (f *fakeEgress) Log(_ context.Context, tenantID string, _ int) ([]egress.LogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, tenantID)
	return []egress.LogEntry{{TenantID: tenantID, Host: "example.com"}}, nil
}
func (f *fakeEgress) Register(string, string, string, string, string) {}
func (f *fakeEgress) Unregister(string)                               {}
func (f *fakeEgress) Close() error                                    { return nil }

var _ egress.Controller = (*fakeEgress)(nil)

// TestEgressNoCrossTenantQuery：admin 有 audit:read / tenant:manage，
// 但这些都是租户级权限，不能用来跨租户读/改出网配置。
func TestEgressNoCrossTenantQuery(t *testing.T) {
	h, _ := newIsolationHandler(t)
	f := &fakeEgress{}
	h.cfg.Egress = f
	// 用 admin：它同时有 audit:read（读日志）与 tenant:manage（改策略），
	// 正是原来两条越权路径的入口。
	admin := auth.Principal{UserID: "u-t1", TenantID: "t1", Role: store.RoleAdmin, Via: auth.ViaJWT}

	do := func(method, path string, body io.Reader) (int, []byte) {
		t.Helper()
		r := httptest.NewRequest(method, path, body)
		r = withPrincipal(r, admin)
		w := httptest.NewRecorder()
		switch {
		case strings.Contains(path, "/egress/log"):
			h.egressLog(w, r)
		case method == http.MethodGet:
			h.egressPolicy(w, r)
		default:
			h.putEgressPolicy(w, r)
		}
		return w.Code, w.Body.Bytes()
	}

	if code, body := do(http.MethodGet, "/v1/admin/egress/log?tenant=t2", nil); code != http.StatusForbidden {
		t.Fatalf("log?tenant=t2 返回 %d，期望 403: %s", code, body)
	}
	if code, body := do(http.MethodGet, "/v1/admin/egress/policy?tenant=t2", nil); code != http.StatusForbidden {
		t.Fatalf("policy?tenant=t2 返回 %d，期望 403: %s", code, body)
	}
	if code, body := do(http.MethodPut, "/v1/admin/egress/policy?tenant=t2",
		strings.NewReader(`{"default":"allow"}`)); code != http.StatusForbidden {
		t.Fatalf("PUT policy?tenant=t2 返回 %d，期望 403: %s", code, body)
	}
	f.mu.Lock()
	if len(f.queried) != 0 || len(f.set) != 0 {
		f.mu.Unlock()
		t.Fatalf("跨租户请求不该触达控制器: queried=%v set=%v", f.queried, f.set)
	}
	f.mu.Unlock()

	// 不带 tenant / 带自己的 tenant：放行，且只查自己。
	for _, q := range []string{"", "?tenant=t1"} {
		if code, body := do(http.MethodGet, "/v1/admin/egress/log"+q, nil); code != http.StatusOK {
			t.Fatalf("log%s 返回 %d: %s", q, code, body)
		}
		if code, body := do(http.MethodGet, "/v1/admin/egress/policy"+q, nil); code != http.StatusOK {
			t.Fatalf("policy%s 返回 %d: %s", q, code, body)
		}
		if code, body := do(http.MethodPut, "/v1/admin/egress/policy"+q,
			strings.NewReader(`{"default":"allow"}`)); code != http.StatusOK {
			t.Fatalf("PUT policy%s 返回 %d: %s", q, code, body)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tid := range append(append([]string{}, f.queried...), f.set...) {
		if tid != "t1" {
			t.Fatalf("控制器收到了别的租户: %v", tid)
		}
	}
}

// ── 3. 全局 LLM 上游 ───────────────────────────────────────

// TestLLMSettingsRequiresPlatformAdmin：租户 owner 有 tenant:manage，
// 但改的是进程级全局上游，必须再过一个平台管理员白名单。
func TestLLMSettingsRequiresPlatformAdmin(t *testing.T) {
	llm := &fakeLLMSettings{key: "sk-old", base: "https://old.example.com"}
	h := newSetupHarness(t, llm)
	_, owner, _, _ := h.seedTenant(t)

	// 白名单为空：只允许 ViaDev，JWT owner 一律 403。
	t.Setenv(platformAdminEnv, "")
	code, body := h.do(t, http.MethodPost, "/v1/admin/llm-settings", owner, map[string]any{
		"apiKey": "sk-evil", "baseUrl": "https://evil.example.com",
	})
	if code != http.StatusForbidden {
		t.Fatalf("白名单为空时 owner 改上游返回 %d，期望 403: %s", code, body)
	}
	if !strings.Contains(string(body), platformAdminEnv) {
		t.Fatalf("403 应提示怎么配 %s: %s", platformAdminEnv, body)
	}
	// test 端点同样被挡（它也会把 baseUrl 连出去）。
	code, _ = h.do(t, http.MethodPost, "/v1/admin/llm-settings/test", owner, map[string]any{
		"apiKey": "sk-x", "baseUrl": "http://127.0.0.1:1",
	})
	if code != http.StatusForbidden {
		t.Fatalf("白名单为空时 test 端点返回 %d，期望 403", code)
	}
	if len(llm.sets) != 0 {
		t.Fatalf("被拒的请求不该改写全局配置: %v", llm.sets)
	}

	// 白名单里有别人（另一个租户的 owner）：403。
	t.Setenv(platformAdminEnv, "other@globex.test")
	if code, _ := h.do(t, http.MethodPost, "/v1/admin/llm-settings", owner,
		map[string]any{"apiKey": "sk-x"}); code != http.StatusForbidden {
		t.Fatalf("不在白名单的 owner 返回 %d，期望 403", code)
	}

	// 大小写 / 空白不敏感。
	t.Setenv(platformAdminEnv, " nobody@x.test , OWNER@ACME.TEST ")
	code, body = h.do(t, http.MethodPost, "/v1/admin/llm-settings", owner, map[string]any{
		"apiKey": "sk-new", "baseUrl": "https://new.example.com",
	})
	if code != http.StatusOK {
		t.Fatalf("白名单命中后应放行，返回 %d: %s", code, body)
	}
	if key, base, _ := llm.Current(); key != "sk-new" || base != "https://new.example.com" {
		t.Fatalf("热更未生效: %q %q", key, base)
	}

	// 读不受影响：owner 仍能看「填了没」。
	if code, body := h.do(t, http.MethodGet, "/v1/admin/llm-settings", owner, nil); code != http.StatusOK {
		t.Fatalf("GET llm-settings 返回 %d，期望 200: %s", code, body)
	}
}

// TestLLMSettingsDevModeAllowed：白名单为空时单机兼容模式仍可配置，
// 否则「clone 下来 make serve」的向导会走不通。
func TestLLMSettingsDevModeAllowed(t *testing.T) {
	llm := &fakeLLMSettings{}
	h := &handler{cfg: Config{LLMSettings: llm}, log: log.New(io.Discard, "", 0)}
	t.Setenv(platformAdminEnv, "")
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/llm-settings",
		strings.NewReader(`{"apiKey":"sk-dev","baseUrl":"https://dev.example.com"}`))
	w := httptest.NewRecorder()
	h.setLLMSettings(w, r) // 无上下文身份 → ViaDev
	if w.Code != http.StatusOK {
		t.Fatalf("dev 模式改上游返回 %d: %s", w.Code, w.Body.String())
	}
	if key, _, _ := llm.Current(); key != "sk-dev" {
		t.Fatalf("dev 模式未生效: %q", key)
	}
}

// ── 4. /v1/admin/obs/traces ────────────────────────────────

type fakeTracer struct {
	mu     sync.Mutex
	filter trace.Filter
	called int
}

func (f *fakeTracer) Enabled() bool                                    { return true }
func (f *fakeTracer) RunStarted(context.Context, trace.Run)            {}
func (f *fakeTracer) RunEnded(context.Context, string, string, string) {}
func (f *fakeTracer) TurnRecorded(context.Context, trace.Turn)         {}
func (f *fakeTracer) ToolCallRecorded(context.Context, trace.ToolCall) {}
func (f *fakeTracer) QueryTraces(_ context.Context, fl trace.Filter) ([]trace.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filter = fl
	f.called++
	return []trace.Summary{{ID: fl.RunID, RunID: fl.RunID, Input: "prompt"}}, nil
}
func (f *fakeTracer) Flush(context.Context) error { return nil }
func (f *fakeTracer) Close() error                { return nil }

var _ trace.Tracer = (*fakeTracer)(nil)

func queryTracesAs(t *testing.T, h *handler, p auth.Principal, query string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/obs/traces"+query, nil)
	r = withPrincipal(r, p)
	w := httptest.NewRecorder()
	h.obsTraces(w, r)
	return w.Code, w.Body.Bytes()
}

// TestObsTracesRequiresOwnedRun：trace 的 Input 是用户原始 prompt，
// 不带 run_id 的全量列表等于把全项目 prompt 发给任意 obs:read 身份。
func TestObsTracesRequiresOwnedRun(t *testing.T) {
	h, st := newIsolationHandler(t)
	seedTenantWorld(t, st)
	tr := &fakeTracer{}
	h.cfg.Trace = tr
	p1 := auth.Principal{UserID: "u-t1", TenantID: "t1", Role: store.RoleAdmin, Via: auth.ViaJWT}

	if code, body := queryTracesAs(t, h, p1, ""); code != http.StatusBadRequest {
		t.Fatalf("不带 run_id 返回 %d，期望 400: %s", code, body)
	}
	if tr.called != 0 {
		t.Fatal("不带 run_id 时不该查询 Langfuse")
	}

	if code, body := queryTracesAs(t, h, p1, "?run_id=r-b"); code != http.StatusNotFound {
		t.Fatalf("别人的 run_id 返回 %d，期望 404: %s", code, body)
	}
	if tr.called != 0 {
		t.Fatal("归属校验失败时不该查询 Langfuse")
	}

	if code, body := queryTracesAs(t, h, p1, "?run_id=ghost"); code != http.StatusNotFound {
		t.Fatalf("不存在的 run_id 返回 %d，期望 404: %s", code, body)
	}

	code, body := queryTracesAs(t, h, p1, "?run_id=r-a&limit=5")
	if code != http.StatusOK {
		t.Fatalf("自己的 run_id 返回 %d: %s", code, body)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.filter.RunID != "r-a" || tr.filter.Limit != 5 {
		t.Fatalf("查询条件不对: %+v", tr.filter)
	}
}
