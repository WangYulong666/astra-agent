package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 假的 ingestion 端点 ─────────────────────────────────────

type capturedEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Body      json.RawMessage `json:"body"`
}

type capture struct {
	mu     sync.Mutex
	reqs   int
	events []capturedEvent
	status int
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != ingestPath {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Batch []capturedEvent `json:"batch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		c.mu.Lock()
		c.status = http.StatusBadRequest
		c.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs++
	c.events = append(c.events, body.Batch...)
	status := c.status
	c.mu.Unlock()
	if status == 0 {
		status = http.StatusMultiStatus
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
}

func (c *capture) snapshot() (int, []capturedEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs, append([]capturedEvent(nil), c.events...)
}

func (c *capture) countType(typ string) int {
	_, evs := c.snapshot()
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func newTestLangfuse(t *testing.T, srv *httptest.Server, rate float64) *Langfuse {
	t.Helper()
	tr, err := openLangfuse(Config{
		Endpoint:   srv.URL,
		PublicKey:  "pk-test",
		SecretKey:  "sk-test",
		SampleRate: rate,
	})
	if err != nil {
		t.Fatalf("openLangfuse: %v", err)
	}
	lf, ok := tr.(*Langfuse)
	if !ok {
		t.Fatalf("期望 *Langfuse，得到 %T", tr)
	}
	t.Cleanup(func() { _ = lf.Close() })
	return lf
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// ── 1. 攒够 20 条自动发出，body 结构正确 ─────────────────────

func TestLangfuseBatchesBySize(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 1)
	ctx := context.Background()

	// RunStarted 产出 2 条（trace-create + 根 span），再补 18 条工具调用 = 20。
	lf.RunStarted(ctx, Run{
		RunID: "run-batch", SessionID: "s1", TenantID: "t1", UserID: "u1",
		Agent: "shell", Provider: "deepseek", Input: "帮我看看 makefile",
		StartedAt: time.Now().UTC(),
	})
	for i := 0; i < 18; i++ {
		lf.ToolCallRecorded(ctx, ToolCall{
			RunID: "run-batch", ToolCallID: fmt.Sprintf("tc-%d", i), Name: "read_file",
			Input: `{"path":"Makefile"}`, Output: "ok", Decision: "allow",
			StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
		})
	}

	waitFor(t, "攒够 20 条后自动上报", func() bool {
		_, evs := cap.snapshot()
		return len(evs) >= 20
	})

	reqs, evs := cap.snapshot()
	if reqs != 1 {
		t.Fatalf("期望 1 次 POST，得到 %d 次", reqs)
	}
	if len(evs) != 20 {
		t.Fatalf("期望 20 条事件，得到 %d", len(evs))
	}
	seen := map[string]bool{}
	for i, ev := range evs {
		if ev.ID == "" || ev.Type == "" || ev.Timestamp == "" || len(ev.Body) == 0 {
			t.Fatalf("第 %d 条事件字段不完整: %+v", i, ev)
		}
		if seen[ev.ID] {
			t.Fatalf("事件 id 重复: %s", ev.ID)
		}
		seen[ev.ID] = true
		if _, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err != nil {
			t.Fatalf("timestamp 不是 RFC3339: %q", ev.Timestamp)
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(ev.Body, &body); err != nil || body.ID == "" {
			t.Fatalf("body 缺少 id: %s", string(ev.Body))
		}
	}

	// 首条必须是 trace-create，且 trace id 就是 runID、name 是 agent:<label>。
	var first struct {
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Input    string         `json:"input"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(evs[0].Body, &first); err != nil {
		t.Fatalf("解析首条事件: %v", err)
	}
	if evs[0].Type != "trace-create" {
		t.Fatalf("首条类型期望 trace-create，得到 %s", evs[0].Type)
	}
	if first.ID != "run-batch" || first.Name != "agent:shell" || first.Input != "帮我看看 makefile" {
		t.Fatalf("trace body 不符: %+v", first)
	}
	for _, k := range []string{"tenant", "user", "session", "provider"} {
		if _, ok := first.Metadata[k]; !ok {
			t.Fatalf("trace metadata 缺少 %s: %+v", k, first.Metadata)
		}
	}
	if evs[1].Type != "span-create" {
		t.Fatalf("第二条期望 span-create，得到 %s", evs[1].Type)
	}
}

// ── 2. Flush 立即发 ─────────────────────────────────────────

func TestLangfuseFlushSendsImmediately(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 1)
	lf.RunStarted(context.Background(), Run{RunID: "run-flush", Input: "hi"})

	if reqs, _ := cap.snapshot(); reqs != 0 {
		t.Fatalf("Flush 前不该有请求，已有 %d 次", reqs)
	}
	if err := lf.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	reqs, evs := cap.snapshot()
	if reqs != 1 {
		t.Fatalf("Flush 后期望 1 次请求，得到 %d", reqs)
	}
	if len(evs) != 2 {
		t.Fatalf("期望 2 条事件，得到 %d", len(evs))
	}
	// Flush 必须等请求真的发完：返回后队列要为空。
	lf.mu.Lock()
	n := len(lf.queue)
	lf.mu.Unlock()
	if n != 0 {
		t.Fatalf("Flush 返回后队列还有 %d 条", n)
	}
}

// ── 3. 端点 500：重试后放弃，不 panic ───────────────────────

func TestLangfuseRetriesThenGivesUp(t *testing.T) {
	cap := &capture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 1)
	lf.RunStarted(context.Background(), Run{RunID: "run-500", Input: "boom"})

	// 允许 Flush 返回错误（这就是「最终失败」的语义），关键是不 panic、不卡死。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := lf.Flush(ctx)
	if err == nil {
		t.Fatalf("端点一直 500，Flush 应该返回错误")
	}
	reqs, _ := cap.snapshot()
	if reqs < 2 {
		t.Fatalf("期望至少重试 1 次，实际只有 %d 次请求", reqs)
	}
	if reqs > maxAttempts {
		t.Fatalf("重试次数超过上限 %d：%d", maxAttempts, reqs)
	}
}

// ── 4. SampleRate=0 一条都不发 ──────────────────────────────

func TestLangfuseSampleRateZeroDropsEverything(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	lf.RunStarted(ctx, Run{RunID: "run-s0", Input: "hi", StartedAt: now})
	lf.TurnRecorded(ctx, Turn{RunID: "run-s0", Input: "hi", Output: "hello", StartedAt: now, EndedAt: now})
	lf.ToolCallRecorded(ctx, ToolCall{RunID: "run-s0", ToolCallID: "t1", Name: "read", StartedAt: now, EndedAt: now})
	lf.RunEnded(ctx, "run-s0", store.RunCompleted, "end_turn")

	if err := lf.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if reqs, evs := cap.snapshot(); reqs != 0 || len(evs) != 0 {
		t.Fatalf("采样率 0 时不该上报，实际 %d 次请求 / %d 条事件", reqs, len(evs))
	}
}

// ── 5. 配置不全时 Open 返回 error（由 app 层降级） ────────────

func TestOpenLangfuseRejectsIncompleteConfig(t *testing.T) {
	if _, err := Open(Config{Endpoint: "http://127.0.0.1:3000"}); err == nil {
		t.Fatalf("缺 key 时 Open 应该报错")
	}
	if _, err := Open(Config{Endpoint: "127.0.0.1:3000", PublicKey: "pk", SecretKey: "sk"}); err == nil {
		t.Fatalf("endpoint 缺 scheme 时 Open 应该报错")
	}
}

// ── 6. Attach：事件流 → trace / generation / span ────────────

type fakeSource struct {
	run  store.Run
	sess store.Session
}

func (f fakeSource) GetRun(context.Context, string) (store.Run, error) { return f.run, nil }
func (f fakeSource) GetSession(context.Context, string) (store.Session, error) {
	return f.sess, nil
}

func ev(runID string, seq int64, typ string, payload any) store.Event {
	raw, _ := json.Marshal(payload)
	return store.Event{RunID: runID, Type: typ, Seq: seq, Payload: raw, CreatedAt: time.Now().UTC()}
}

func TestLangfuseAttachMapsRunLifecycle(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 1)
	hub := eventbus.New()

	start := time.Now().UTC().Add(-2 * time.Second)
	src := fakeSource{
		run:  store.Run{ID: "run-attach", SessionID: "sess-1", Input: "把结果写进 notes.md", Status: store.RunRunning, StartedAt: start},
		sess: store.Session{ID: "sess-1", TenantID: "tenant-1", UserID: "user-1"},
	}
	stop := lf.Attach(hub, src)

	hub.Publish(ev("run-attach", 1, store.EventState, map[string]any{"status": store.RunRunning}))
	hub.Publish(ev("run-attach", 2, store.EventToolStart, map[string]any{
		"toolCallId": "call-1", "title": "write_file", "kind": "edit",
		"rawInput": json.RawMessage(`{"path":"notes.md"}`),
	}))
	hub.Publish(ev("run-attach", 3, store.EventApprovalResult, map[string]any{
		"toolCallId": "call-1", "decision": "allow",
	}))
	hub.Publish(ev("run-attach", 4, store.EventToolUpdate, map[string]any{
		"toolCallId": "call-1", "status": "completed", "rawOutput": "written",
	}))
	hub.Publish(ev("run-attach", 5, store.EventMessageDelta, map[string]any{"text": "已写入 "}))
	hub.Publish(ev("run-attach", 6, store.EventMessageDelta, map[string]any{"text": "notes.md"}))
	hub.Publish(ev("run-attach", 7, store.EventState, map[string]any{
		"status": store.RunCompleted, "stopReason": "end_turn",
	}))

	stop() // 收工并 Flush

	_, evs := cap.snapshot()
	if len(evs) == 0 {
		t.Fatalf("Attach 后没有上报任何事件")
	}
	if n := cap.countType("trace-create"); n < 2 {
		t.Fatalf("期望 trace-create 至少 2 条（创建 + 收口），得到 %d", n)
	}
	if n := cap.countType("generation-create"); n != 1 {
		t.Fatalf("期望 1 条 generation，得到 %d", n)
	}
	if n := cap.countType("span-create"); n < 2 {
		t.Fatalf("期望根 span + 工具 span，得到 %d 条 span-create", n)
	}
	if n := cap.countType("span-update"); n != 1 {
		t.Fatalf("期望 1 条 span-update（run 结束），得到 %d", n)
	}

	var gen, span, end struct {
		ID      string         `json:"id"`
		TraceID string         `json:"traceId"`
		Input   string         `json:"input"`
		Output  string         `json:"output"`
		EndTime string         `json:"endTime"`
		Level   string         `json:"level"`
		Status  string         `json:"status"`
		Meta    map[string]any `json:"metadata"`
	}
	for _, e := range evs {
		switch e.Type {
		case "generation-create":
			_ = json.Unmarshal(e.Body, &gen)
		case "span-create":
			if strings.Contains(string(e.Body), "call-1") {
				_ = json.Unmarshal(e.Body, &span)
			}
		case "span-update":
			_ = json.Unmarshal(e.Body, &end)
		}
	}
	if gen.TraceID != "run-attach" || gen.Input != "把结果写进 notes.md" || gen.Output != "已写入 notes.md" {
		t.Fatalf("generation 映射不符: %+v", gen)
	}
	if span.TraceID != "run-attach" || !strings.Contains(span.ID, "call-1") {
		t.Fatalf("工具 span 映射不符: %+v", span)
	}
	if d, _ := span.Meta["decision"].(string); d != "allow" {
		t.Fatalf("工具 span 的审批决策应为 allow，得到 %v", span.Meta)
	}
	if end.EndTime == "" || end.Meta["status"] != store.RunCompleted {
		t.Fatalf("span-update 应带 end_time 和 status: %+v", end)
	}
}

// ── 7. QueryTraces 映射 ─────────────────────────────────────

func TestLangfuseQueryTraces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(ingestPath, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusMultiStatus) })
	mux.HandleFunc(tracesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("limit 未透传: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"run-q","name":"agent:shell","timestamp":"2026-01-02T03:04:05Z","latency":1.5,"input":"hi","metadata":{"status":"completed","run_id":"run-q"}}]}`))
	})
	mux.HandleFunc(tracesPath+"/run-q", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"run-q","name":"agent:shell","timestamp":"2026-01-02T03:04:05Z","latency":2,"metadata":{"status":"completed"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lf := newTestLangfuse(t, srv, 1)
	ctx := context.Background()

	list, err := lf.QueryTraces(ctx, Filter{Limit: 5})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(list))
	}
	got := list[0]
	if got.ID != "run-q" || got.RunID != "run-q" || got.Status != "completed" {
		t.Fatalf("列表映射不符: %+v", got)
	}
	if got.LatencyMS != 1500 {
		t.Fatalf("latency 应换算成毫秒 1500，得到 %d", got.LatencyMS)
	}
	if got.URL != srv.URL+"/trace/run-q" {
		t.Fatalf("URL 不符: %s", got.URL)
	}
	if !got.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("started_at 不符: %v", got.StartedAt)
	}

	one, err := lf.QueryTraces(ctx, Filter{RunID: "run-q"})
	if err != nil || len(one) != 1 || one[0].LatencyMS != 2000 {
		t.Fatalf("按 runID 查询不符: %+v, err=%v", one, err)
	}
}
