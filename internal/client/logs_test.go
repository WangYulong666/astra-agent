package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 日志类查询的协议翻译测试：路径、查询参数、Bearer 头、包装对象解析。
// 全部用 httptest，不打真控制面。

type logProbe struct {
	path  string
	query string
	auth  string
}

func newLogServer(t *testing.T, body string, p *logProbe) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.path = r.URL.Path
		p.query = r.URL.RawQuery
		p.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
}

func TestAdminEgressLog(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"items":[{"time":"2026-11-07T01:02:03Z","tenant_id":"t1","run_id":"r1","host":"api.example.com","port":"443","decision":"deny","reason":"default","approval_id":"ap1"}],"limit":7}`, &p)
	defer srv.Close()
	c := New(srv.URL, "tok")
	items, err := c.AdminEgressLog(context.Background(), 7, "t1")
	if err != nil {
		t.Fatalf("AdminEgressLog: %v", err)
	}
	if p.path != "/v1/admin/egress/log" || !strings.Contains(p.query, "limit=7") || !strings.Contains(p.query, "tenant=t1") {
		t.Fatalf("请求不对: %s?%s", p.path, p.query)
	}
	if p.auth != "Bearer tok" {
		t.Fatalf("缺 Bearer 头: %q", p.auth)
	}
	if len(items) != 1 || items[0].Decision != "deny" || items[0].ApprovalID != "ap1" {
		t.Fatalf("解析不对: %+v", items)
	}
}

func TestAdminEgressLogEmptyIsSliceNotNil(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"items":null,"limit":100}`, &p)
	defer srv.Close()
	items, err := New(srv.URL, "tok").AdminEgressLog(context.Background(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("空结果应是空切片，实际 %#v", items)
	}
	if !strings.Contains(p.query, "limit=100") {
		t.Fatalf("默认 limit 应为 100: %s", p.query)
	}
}

func TestRunActivityAndSessionActivity(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"runId":"r1","entries":[{"time":"2026-11-07T01:00:00Z","kind":"tool","title":"bash","status":"done"}],"summary":{"done":1,"running":0,"blocked":1,"failed":0,"current":"等待审批"}}`, &p)
	defer srv.Close()
	c := New(srv.URL, "tok")

	out, err := c.RunActivity(context.Background(), "r1", 50)
	if err != nil {
		t.Fatalf("RunActivity: %v", err)
	}
	if p.path != "/v1/runs/r1/activity" || !strings.Contains(p.query, "limit=50") {
		t.Fatalf("run 视图路径不对: %s?%s", p.path, p.query)
	}
	if out.Summary == nil || out.Summary.Blocked != 1 || out.Summary.Current != "等待审批" {
		t.Fatalf("summary 解析不对: %+v", out.Summary)
	}
	if len(out.Entries) != 1 || out.Entries[0].Kind != "tool" {
		t.Fatalf("entries 解析不对: %+v", out.Entries)
	}

	sess, err := c.SessionActivity(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("SessionActivity: %v", err)
	}
	if p.path != "/v1/sessions/s1/activity" {
		t.Fatalf("session 视图路径不对: %s", p.path)
	}
	if p.query != "" {
		t.Fatalf("limit=0 不该带参数: %s", p.query)
	}
	if sess.Entries == nil {
		t.Fatal("entries 应归一化为空切片")
	}
}

func TestRunActivityRequiresID(t *testing.T) {
	c := New("http://127.0.0.1:1", "tok")
	if _, err := c.RunActivity(context.Background(), "", 0); !IsUseError(err) {
		t.Fatalf("缺 run id 应返回 UseError，实际 %v", err)
	}
	if _, err := c.SessionActivity(context.Background(), "", 0); !IsUseError(err) {
		t.Fatalf("缺 session id 应返回 UseError，实际 %v", err)
	}
	if _, err := c.RunChildren(context.Background(), ""); !IsUseError(err) {
		t.Fatalf("缺 run id 应返回 UseError，实际 %v", err)
	}
}

func TestRunChildren(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `[{"sessionId":"s2","runId":"r2","parentRunId":"r1","status":"completed","summary":"done","depth":1}]`, &p)
	defer srv.Close()
	items, err := New(srv.URL, "tok").RunChildren(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if p.path != "/v1/runs/r1/children" {
		t.Fatalf("路径不对: %s", p.path)
	}
	if len(items) != 1 || items[0].Depth != 1 || items[0].Summary != "done" {
		t.Fatalf("解析不对: %+v", items)
	}
}

func TestUsageUsesUserEndpoint(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"items":[{"id":"u1","model":"deepseek","inputTokens":10,"outputTokens":5,"costMicroUsd":100}],"sum":{"inputTokens":10,"outputTokens":5,"costMicroUsd":100,"calls":1}}`, &p)
	defer srv.Close()
	report, err := New(srv.URL, "tok").Usage(context.Background(), UsageQuery{From: "2026-11-01", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	// 与 AdminUsage 的区别就在路径：这个是 /v1/usage，不是 /v1/admin/usage。
	if p.path != "/v1/usage" {
		t.Fatalf("应打 /v1/usage，实际 %s", p.path)
	}
	if !strings.Contains(p.query, "from=2026-11-01") || !strings.Contains(p.query, "limit=20") {
		t.Fatalf("参数不对: %s", p.query)
	}
	if report.Sum.Calls != 1 || report.Sum.TotalTokens() != 15 {
		t.Fatalf("汇总解析不对: %+v", report.Sum)
	}
}

func TestBilling(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"month":"2026-11","usedMicroUsd":12345}`, &p)
	defer srv.Close()
	out, err := New(srv.URL, "tok").Billing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.path != "/v1/usage/billing" {
		t.Fatalf("路径不对: %s", p.path)
	}
	if out["month"] != "2026-11" {
		t.Fatalf("解析不对: %+v", out)
	}
}

func TestAdminInsights(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"items":[{"runId":"r1","sessionId":"s1","status":"failed","error":"exec: timeout","lastTool":"bash","suggestion":"缩短超时或拆分步骤","toolCount":3,"blocked":1,"failed":1,"createdAt":"2026-11-07T01:02:03Z"}],"limit":7}`, &p)
	defer srv.Close()
	items, err := New(srv.URL, "tok").AdminInsights(context.Background(), "s1", 7)
	if err != nil {
		t.Fatalf("AdminInsights: %v", err)
	}
	if p.path != "/v1/insights" || !strings.Contains(p.query, "limit=7") || !strings.Contains(p.query, "session_id=s1") {
		t.Fatalf("请求不对: %s?%s", p.path, p.query)
	}
	if p.auth != "Bearer tok" {
		t.Fatalf("缺 Bearer 头: %q", p.auth)
	}
	if len(items) != 1 {
		t.Fatalf("解析条数不对: %+v", items)
	}
	got := items[0]
	if got.RunID != "r1" || got.SessionID != "s1" || got.Status != "failed" ||
		got.Error != "exec: timeout" || got.LastTool != "bash" || got.Suggestion != "缩短超时或拆分步骤" ||
		got.ToolCount != 3 || got.Blocked != 1 || got.Failed != 1 {
		t.Fatalf("字段解析不对: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.CreatedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-07T01:02:03Z" {
		t.Fatalf("createdAt 解析不对: %v", got.CreatedAt)
	}
}

func TestAdminInsightsDefaultsAndClamp(t *testing.T) {
	var p logProbe
	srv := newLogServer(t, `{"items":null,"limit":50}`, &p)
	defer srv.Close()
	c := New(srv.URL, "tok")

	items, err := c.AdminInsights(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("空结果应是空切片，实际 %#v", items)
	}
	if !strings.Contains(p.query, "limit=50") || strings.Contains(p.query, "session_id") {
		t.Fatalf("默认 limit=50 且不带 session_id: %s", p.query)
	}

	if _, err := c.AdminInsights(context.Background(), "", 9999); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.query, "limit=500") {
		t.Fatalf("超过上限应 clamp 到 500: %s", p.query)
	}
}
