package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这组测试只覆盖「协议翻译」部分：SSE 解析、错误映射、参数编码、文件落盘、
// 分页适配、鉴权头。它们全用 httptest 模拟服务端，不打真机——
// 真机验证在汇报里单独给出（whoami / session list / run --wait）。

func TestSSEParseStreamsDeltasAndToolCalls(t *testing.T) {
	// 服务端真实格式（见 internal/api/api.go writeEvent + events）：
	// 先发 retry，再逐条 id/event/data，字段之间用 "\n"，事件之间用空行。
	const body = "retry: 1000\n\n" +
		": ping\n\n" +
		"id: 1\nevent: message_delta\ndata: {\"text\":\"你好\"}\n\n" +
		"id: 2\nevent: tool_call_start\ndata: {\"toolCallId\":\"tc1\",\"title\":\"bash\",\"kind\":\"execute\",\"status\":\"pending\",\"rawInput\":{\"command\":\"echo hi\"}}\n\n" +
		"id: 3\nevent: message_delta\ndata: {\"text\":\"hi\\n\"}\n\n" +
		"id: 4\nevent: run_state\ndata: {\"status\":\"completed\"}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/runs/r1/events" {
			t.Errorf("路径错误: %s", got)
		}
		if got := r.URL.Query().Get("after_seq"); got != "7" {
			t.Errorf("after_seq 应为 7，实际 %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("缺少 Bearer 头: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	ch, err := c.RunEvents(context.Background(), "r1", 7)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}
	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 4 {
		t.Fatalf("应收到 4 条事件，实际 %d: %+v", len(got), got)
	}
	if got[0].Type != EventMessageDelta || got[0].Text() != "你好" || got[0].Seq != 1 {
		t.Errorf("第一条事件解析错误: %+v", got[0])
	}
	tc := got[1].Tool()
	if got[1].Type != EventToolStart || tc.ToolCallID != "tc1" || tc.Title != "bash" {
		t.Errorf("工具事件解析错误: %+v", got[1])
	}
	if !strings.Contains(string(tc.RawInput), "echo hi") {
		t.Errorf("rawInput 丢失: %s", tc.RawInput)
	}
	last := got[3]
	if !last.IsTerminal() || last.Status() != "completed" {
		t.Errorf("终态事件应被识别: %+v", last)
	}
}

// 多行 data 必须按 SSE 规范用 \n 拼起来，否则解析出半个 JSON。
func TestSSEParseMultilineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_delta\ndata: {\"text\":\"line1\"}\n\n")
		fmt.Fprint(w, "event: agent_log\ndata: {\"error\":\"a\",\ndata: \"b\"}\n\n")
	}))
	defer srv.Close()

	ch, err := New(srv.URL, "t").RunEvents(context.Background(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for ev := range ch {
		types = append(types, ev.Type)
	}
	if len(types) != 2 {
		t.Fatalf("应收到 2 条事件，实际 %v", types)
	}
}

func TestErrorMappingUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"需要登录"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "bad").Meta(context.Background())
	if err == nil {
		t.Fatal("401 应该返回错误")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型应为 *APIError，实际 %T", err)
	}
	if ae.Status != 401 || ae.Message != "需要登录" {
		t.Errorf("错误字段不对: %+v", ae)
	}
	// 人话里要带「重新登录」，否则用户只知道失败不知道怎么修。
	if msg := err.Error(); !strings.Contains(msg, "重新登录") || !strings.Contains(msg, "401") {
		t.Errorf("401 提示不够可操作: %s", msg)
	}
	if !IsUnauthorized(err) {
		t.Error("IsUnauthorized 应识别 401")
	}
}

// 路由缺失时服务端的 SPA 兜底会回 200 + HTML。必须报成「路由未注册」，
// 而不是让用户看到 invalid character '<'。
func TestNonJSONResponseIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><html></html>")
	}))
	defer srv.Close()

	_, err := New(srv.URL, "t").MemoryStats(context.Background())
	if err == nil {
		t.Fatal("HTML 响应应该报错")
	}
	if !strings.Contains(err.Error(), "不是 JSON") {
		t.Errorf("错误信息应说明返回的不是 JSON: %v", err)
	}
}

// memory 的参数编码：q/k/kind 进 query，k 缺省补 10。
func TestMemorySearchEncodesParams(t *testing.T) {
	var gotQ, gotK, gotKind string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/memory/search" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		gotQ = r.URL.Query().Get("q")
		gotK = r.URL.Query().Get("k")
		gotKind = r.URL.Query().Get("kind")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"m1","kind":"fact","text":"用户偏好中文","score":0.8123,"created_at":"2026-11-01T10:00:00Z"}],"query":"上次的方案","k":8}`)
	}))
	defer srv.Close()

	items, err := New(srv.URL, "t").MemorySearch(context.Background(), "上次的方案", 8, "fact")
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != "上次的方案" || gotK != "8" || gotKind != "fact" {
		t.Errorf("参数编码错误: q=%q k=%q kind=%q", gotQ, gotK, gotKind)
	}
	if len(items) != 1 || items[0].ID != "m1" || items[0].Score < 0.81 || items[0].Score > 0.82 {
		t.Errorf("响应解析错误: %+v", items)
	}

	// k<=0 时必须补默认值，否则服务端会 400。
	_, _ = New(srv.URL, "t").MemorySearch(context.Background(), "x", 0, "")
	if gotK != "10" {
		t.Errorf("k<=0 应补 10，实际 %q", gotK)
	}
}

func TestDownloadFileWritesRawBytes(t *testing.T) {
	payload := []byte("col1,col2\n1,2\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/s1/download" {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("path"); got != "out/report.csv" {
			t.Errorf("path 编码错误: %q", got)
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	n, err := New(srv.URL, "t").DownloadFile(context.Background(), "s1", "out/report.csv", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) || !bytes.Equal(buf.Bytes(), payload) {
		t.Errorf("内容不一致: n=%d body=%q", n, buf.String())
	}
}

func TestDownloadFileErrorIsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"文件不存在: nope.txt"}`)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	_, err := New(srv.URL, "t").DownloadFile(context.Background(), "s1", "nope.txt", &buf)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("应返回 404 APIError，实际 %v", err)
	}
	if buf.Len() != 0 {
		t.Error("失败时不该写任何字节")
	}
}

// admin 分页：服务端只支持 limit，不支持 offset。客户端用
// 「limit=offset+limit 再切掉前 offset 条」实现翻页。
func TestAdminAuditPagination(t *testing.T) {
	var gotLimit string
	mkLogs := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{
				"id":        fmt.Sprintf("a%d", i),
				"action":    "session.create",
				"tenantId":  "t1",
				"createdAt": time.Date(2026, 11, 1, 0, i, 0, 0, time.UTC),
			}
		}
		return out
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		if got := r.URL.Query().Get("action"); got != "" && got != "session.create" {
			t.Errorf("action 过滤传错: %q", got)
		}
		// 模拟服务端：只认 limit，按 DESC 返回最多 limit 条。
		n := 12
		var lim int
		if _, err := fmt.Sscanf(gotLimit, "%d", &lim); err == nil && lim > 0 && lim < n {
			n = lim
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mkLogs(n))
	}))
	defer srv.Close()

	logs, err := New(srv.URL, "t").AdminAudit(context.Background(), AuditQuery{
		Action: "session.create", Limit: 5, Offset: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotLimit != "10" {
		t.Errorf("limit 应为 offset+limit=10，实际 %q", gotLimit)
	}
	if len(logs) != 5 || logs[0].ID != "a5" {
		t.Fatalf("分页切片错误: len=%d first=%v", len(logs), logs)
	}

	// 翻过末尾时返回空集合（而不是 nil 或错误），脚本才好判断「没有更多」。
	logs, err = New(srv.URL, "t").AdminAudit(context.Background(), AuditQuery{Limit: 5, Offset: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Errorf("越界应返回空集合，实际 %d 条", len(logs))
	}
}

// 建会话：POST 只接受 cwd，标题必须用 PATCH 补——两次请求都要发生。
func TestCreateSessionSetsTitleWithPatch(t *testing.T) {
	var posted, patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			posted = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"s1","cwd":"/w","status":"active","createdAt":"2026-11-01T00:00:00Z"}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/sessions/s1":
			patched = true
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["title"] != "排查构建失败" {
				t.Errorf("PATCH body 里没有 title: %v", body)
			}
			fmt.Fprint(w, `{"id":"s1","title":"排查构建失败","cwd":"/w","status":"active"}`)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	s, err := New(srv.URL, "t").CreateSession(context.Background(), "排查构建失败")
	if err != nil {
		t.Fatal(err)
	}
	if !posted || !patched {
		t.Errorf("POST=%v PATCH=%v，两个都要有", posted, patched)
	}
	if s.Title != "排查构建失败" {
		t.Errorf("标题没生效: %+v", s)
	}
}

// 审批决定只接受 allow / deny，合法性在客户端就拦住（别把脏值发给服务端）。
func TestResolveApprovalValidatesDecision(t *testing.T) {
	c := New("http://127.0.0.1:1", "t")
	err := c.ResolveApproval(context.Background(), "r1", "tc1", "maybe")
	if err == nil {
		t.Fatal("非法 decision 应该报错")
	}
	if !IsUseError(&UseError{Msg: err.Error()}) && !strings.Contains(err.Error(), "allow") {
		t.Errorf("错误应提示合法取值: %v", err)
	}
}
