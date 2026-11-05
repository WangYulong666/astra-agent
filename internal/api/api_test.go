package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

var (
	proxyBin string
	mockBin  string
	binErr   error
)

// 现编真二进制而不是打桩：这条链路的坑都在进程/连接边界上
// （端口分配、WS 分帧、SSE flush），fake 恰好绕过它们。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "astra-api-bin")
	if err != nil {
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	proxyBin = filepath.Join(dir, "acp-proxy")
	mockBin = filepath.Join(dir, "acp-mock-agent")
	for out, pkg := range map[string]string{
		proxyBin: "../../cmd/acp-proxy",
		mockBin:  "../../cmd/acp-mock-agent",
	} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			binErr = err
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

type harness struct {
	srv *httptest.Server
	st  *store.File
	m   *runner.Manager
	p   sandbox.Provider
	ws  string // local provider 的沙箱根目录
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if binErr != nil {
		t.Fatalf("准备二进制失败: %v", binErr)
	}

	root := t.TempDir()
	st, err := store.OpenFile(filepath.Join(root, "data"), store.Options{})
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	p, err := sandbox.New("local", sandbox.Options{
		Root:     filepath.Join(root, "sandboxes"),
		ProxyBin: proxyBin,
	})
	if err != nil {
		t.Fatalf("建 provider: %v", err)
	}

	hub := eventbus.New()
	m, err := runner.New(runner.Config{
		Store:    st,
		Provider: p,
		Hub:      hub,
		Agent: sandbox.AgentSpec{
			Command: []string{mockBin},
			Env:     map[string]string{"ASTRA_MOCK_STATE": filepath.Join(root, "mock-state")},
		},
		CoalesceWindow: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("建 runner: %v", err)
	}

	h, err := Handler(Config{Manager: m, Store: st, Hub: hub, Provider: p, Heartbeat: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("建 handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		_ = m.Close()
		_ = st.Close()
	})
	return &harness{
		srv: srv,
		st:  st,
		m:   m,
		p:   p,
		ws:  filepath.Join(root, "sandboxes"),
	}
}

func (h *harness) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) newSession(t *testing.T) store.Session {
	t.Helper()
	code, body := h.do(t, http.MethodPost, "/v1/sessions", map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("建会话返回 %d: %s", code, body)
	}
	var s store.Session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// sseEvent 是解析后的 SSE 帧。
type sseEvent struct {
	ID   int64
	Type string
	Data string
}

// readSSE 读到连接关闭或 ctx 超时。返回全部事件。
func readSSE(t *testing.T, ctx context.Context, url string) []sseEvent {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，不是 SSE", ct)
	}

	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Type != "" || cur.Data != "" {
				events = append(events, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.ID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return events
}

// TestEventsReplayAndClose 验证「run 已结束」时 SSE 不要挂住。
//
// 这条最容易写错：如果只有实时通道而没有「终态即返回」的逻辑，
// 客户端拿完数据后连接会一直开着，浏览器 EventSource 也不会自己停。
func TestEventsReplayAndClose(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	code, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "跑一下"})
	if code != http.StatusCreated {
		t.Fatalf("建 run 返回 %d: %s", code, body)
	}
	var run store.Run
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatal(err)
	}

	// 等 run 结束再连 SSE —— 纯回放路径。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := h.st.GetRun(context.Background(), run.ID)
		if err == nil && store.IsTerminal(r.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evs := readSSE(t, ctx, h.srv.URL+"/v1/runs/"+run.ID+"/events")

	if ctx.Err() != nil {
		t.Fatal("run 已结束，SSE 却一直不关闭（缺「终态即返回」）")
	}

	var text strings.Builder
	var sawCompleted bool
	var lastID int64
	for _, e := range evs {
		if e.ID <= lastID && e.ID != 0 {
			t.Errorf("seq 不单调: %d 在 %d 之后", e.ID, lastID)
		}
		lastID = e.ID
		switch e.Type {
		case store.EventMessageDelta:
			var p struct{ Text string }
			_ = json.Unmarshal([]byte(e.Data), &p)
			text.WriteString(p.Text)
		case store.EventState:
			var p struct{ Status string }
			_ = json.Unmarshal([]byte(e.Data), &p)
			if store.IsTerminal(p.Status) {
				sawCompleted = true
			}
		}
	}
	if got := strings.TrimSpace(text.String()); got != "已处理：跑一下" {
		t.Errorf("回放的文本 = %q", got)
	}
	if !sawCompleted {
		t.Error("回放里没有终态事件")
	}
	if lastID == 0 {
		t.Error("事件没有 id，客户端断线后无法续传")
	}
}

// TestEventsAfterSeq 验证续传：after_seq 之后只返回新事件。
func TestEventsAfterSeq(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	_, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "断点"})
	var run store.Run
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := h.st.GetRun(context.Background(), run.ID); err == nil && store.IsTerminal(r.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	all := readSSE(t, ctx, h.srv.URL+"/v1/runs/"+run.ID+"/events")
	if len(all) < 3 {
		t.Fatalf("事件太少: %d", len(all))
	}

	cut := all[1].ID
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	rest := readSSE(t, ctx2, h.srv.URL+"/v1/runs/"+run.ID+"/events?after_seq="+strconv.FormatInt(cut, 10))
	if len(rest) == 0 {
		t.Fatal("after_seq 之后没有事件")
	}
	for _, e := range rest {
		if e.ID <= cut {
			t.Errorf("after_seq=%d 却返回了 seq=%d", cut, e.ID)
		}
	}
	if len(rest) != len(all)-2 {
		t.Errorf("续传条数 = %d，期望 %d", len(rest), len(all)-2)
	}
}

// TestSessionLifecycle 覆盖建/查/删与 404。
func TestSessionLifecycle(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	if code, _ := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID, nil); code != http.StatusOK {
		t.Errorf("查会话返回 %d", code)
	}
	if code, _ := h.do(t, http.MethodDelete, "/v1/sessions/"+sess.ID, nil); code != http.StatusNoContent {
		t.Errorf("删会话返回 %d", code)
	}
	if code, _ := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID, nil); code != http.StatusNotFound {
		t.Errorf("删后查会话返回 %d，期望 404", code)
	}
	if code, _ := h.do(t, http.MethodGet, "/v1/runs/nonexistent/events", nil); code != http.StatusNotFound {
		t.Errorf("不存在的 run 取事件返回 %d，期望 404", code)
	}
}

// TestCreateRunValidation 空输入必须被挡住，否则会起一个空 prompt 的 run。
func TestCreateRunValidation(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)
	code, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "   "})
	if code != http.StatusBadRequest {
		t.Fatalf("空 input 返回 %d: %s", code, body)
	}
}

// ── 新增接口：历史 / 文件 / 重命名 / 审批 ────────────────────

func TestHistoryJSON(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	code, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "跑一下"})
	if code != http.StatusCreated {
		t.Fatalf("建 run 返回 %d: %s", code, body)
	}
	var run store.Run
	_ = json.Unmarshal(body, &run)

	// 等它跑完，再拉历史。history 是给首屏用的，必须包含全部事件（含终态）。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := h.st.GetRun(context.Background(), run.ID)
		if err == nil && store.IsTerminal(cur.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, body = h.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/history", nil)
	if code != http.StatusOK {
		t.Fatalf("history 返回 %d: %s", code, body)
	}
	var evs []store.Event
	if err := json.Unmarshal(body, &evs); err != nil {
		t.Fatalf("history 不是事件数组: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("history 为空")
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq=%d，应为 %d", i, ev.Seq, i+1)
		}
	}
	last := evs[len(evs)-1]
	if last.Type != store.EventState || !strings.Contains(string(last.Payload), `"status":"completed"`) {
		t.Fatalf("最后一条不是终态: %s %s", last.Type, last.Payload)
	}
}

func TestFilesListAndPreview(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	// 直接往沙箱 workspace 里放文件：这条用例验证的是「读」，不是 agent 会不会写。
	ws := filepath.Join(h.ws, sess.ID, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "reports", "week.md"), []byte("# 周报\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/files", nil)
	if code != http.StatusOK {
		t.Fatalf("列目录返回 %d: %s", code, body)
	}
	var listing struct {
		Path    string      `json:"path"`
		Entries []fileEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || !listing.Entries[0].IsDir || listing.Entries[0].Name != "reports" {
		t.Fatalf("目录项不对: %+v", listing.Entries)
	}

	code, body = h.do(t, http.MethodGet, "/v1/sessions/"+sess.ID+"/file?path=reports/week.md", nil)
	if code != http.StatusOK {
		t.Fatalf("读文件返回 %d: %s", code, body)
	}
	var fc fileContent
	if err := json.Unmarshal(body, &fc); err != nil {
		t.Fatal(err)
	}
	if fc.Content != "# 周报\n" || fc.Binary || fc.Truncated {
		t.Fatalf("文件内容不对: %+v", fc)
	}
}

// 软链逃逸必须挡住：只做 filepath.Join 的实现在这条用例下会把 /etc/passwd 读出来。
func TestFilesRejectEscape(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	ws := filepath.Join(h.ws, sess.ID, "workspace")
	_ = os.MkdirAll(ws, 0o755)
	if err := os.Symlink("/etc", filepath.Join(ws, "escape")); err != nil {
		t.Skipf("建软链失败，跳过: %v", err)
	}

	cases := []string{
		"/v1/sessions/" + sess.ID + "/file?path=../../meta.json",
		"/v1/sessions/" + sess.ID + "/file?path=escape/hosts",
		"/v1/sessions/" + sess.ID + "/files?path=escape",
	}
	for _, c := range cases {
		code, body := h.do(t, http.MethodGet, c, nil)
		if code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("%s 应被拒绝，实际 %d: %s", c, code, body)
		}
	}
}

func TestSessionRename(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	code, body := h.do(t, http.MethodPatch, "/v1/sessions/"+sess.ID, map[string]any{"title": "  整理周报  "})
	if code != http.StatusOK {
		t.Fatalf("重命名返回 %d: %s", code, body)
	}
	var got store.Session
	_ = json.Unmarshal(body, &got)
	if got.Title != "整理周报" {
		t.Fatalf("标题 = %q，应去掉首尾空格", got.Title)
	}

	code, _ = h.do(t, http.MethodPatch, "/v1/sessions/"+sess.ID, map[string]any{"title": "  "})
	if code != http.StatusBadRequest {
		t.Fatalf("空标题应返回 400，实际 %d", code)
	}
}

func TestApprovalOnUnknownToolCall(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(t)

	code, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "跑一下"})
	if code != http.StatusCreated {
		t.Fatalf("建 run 返回 %d: %s", code, body)
	}
	var run store.Run
	_ = json.Unmarshal(body, &run)

	code, body = h.do(t, http.MethodPost, "/v1/runs/"+run.ID+"/approvals/tc-nope", map[string]any{"decision": "allow"})
	if code != http.StatusNotFound {
		t.Fatalf("对不存在的审批应返回 404，实际 %d: %s", code, body)
	}

	code, _ = h.do(t, http.MethodPost, "/v1/runs/"+run.ID+"/approvals/tc-nope", map[string]any{"decision": "maybe"})
	if code != http.StatusInternalServerError && code != http.StatusBadRequest {
		// 非法决策值应该在 ResolveApproval 里被拒；状态码具体是哪个不重要，
		// 重要的是不能当成成功把 200 返回给前端。
		t.Fatalf("非法决策应报错，实际 %d", code)
	}
}
