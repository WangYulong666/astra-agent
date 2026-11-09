package insight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

type testEnv struct {
	dir  string
	st   store.Store
	hub  *eventbus.Hub
	ins  Store
	stop func()
}

func newTestEnv(t *testing.T, act Activity) *testEnv {
	t.Helper()
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	insightDir := filepath.Join(root, "insights")

	fst, err := store.OpenFile(storeDir, store.Options{Sync: false})
	if err != nil {
		t.Fatal(err)
	}
	hub := eventbus.New()
	lg := log.New(io.Discard, "", 0)
	stop := Attach(hub, fst, act, insightDir, lg)
	ins, err := Open(insightDir)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{dir: insightDir, st: fst, hub: hub, ins: ins, stop: stop}
	t.Cleanup(func() {
		stop()
		_ = fst.Close()
	})
	return e
}

func (e *testEnv) makeRun(t *testing.T, sessionID, runID, errText string) {
	t.Helper()
	r := store.Run{
		ID: runID, SessionID: sessionID, Status: store.RunFailed,
		Error: errText, CreatedAt: time.Now().UTC(), NextSeq: 1,
	}
	if err := e.st.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) addTool(t *testing.T, runID, typ, title string) {
	t.Helper()
	payload := map[string]any{"toolCallId": "tc", "title": title, "kind": "execute", "status": "completed"}
	if _, err := e.st.Append(context.Background(), runID, store.NewEvent{Type: typ, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) publish(runID string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	e.hub.Publish(store.Event{RunID: runID, Type: store.EventState, Payload: b})
}

func (e *testEnv) publishRaw(runID string, payload json.RawMessage) {
	e.hub.Publish(store.Event{RunID: runID, Type: store.EventState, Payload: payload})
}

// diagnosis 要求调用方先 e.stop()，stop 的 wg.Wait 保证所有在飞诊断已落库。
func (e *testEnv) diagnosis(t *testing.T, runID string) Diagnosis {
	t.Helper()
	d, err := e.ins.ForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("诊断 %s 未生成: %v", runID, err)
	}
	return d
}

func (e *testEnv) all(t *testing.T) []Diagnosis {
	t.Helper()
	got, err := e.ins.All(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

type fixedActivity struct{ blocked, failed int }

func (a fixedActivity) Summary(context.Context, string) (Summary, error) {
	return Summary{Blocked: a.blocked, Failed: a.failed}, nil
}

var _ Activity = fixedActivity{}

// blockingActivity 尊重 ctx：stop() 的 cancel 会让它退出，用来造「在飞」诊断。
type blockingActivity struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (a *blockingActivity) Summary(ctx context.Context, _ string) (Summary, error) {
	a.once.Do(func() { close(a.entered) })
	select {
	case <-ctx.Done():
	case <-a.release:
	}
	return Summary{}, nil
}

var _ Activity = (*blockingActivity)(nil)

// sleepActivity 不理 ctx、有限慢，用来验证 stop 的 wg.Wait 会老实等它收工。
type sleepActivity time.Duration

func (a sleepActivity) Summary(context.Context, string) (Summary, error) {
	time.Sleep(time.Duration(a))
	return Summary{}, nil
}

var _ Activity = sleepActivity(0)

func TestAttachFailedAndCancelledGenerate(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-failed", "boom")
	e.makeRun(t, "s2", "run-cancelled", "")
	e.publish("run-failed", map[string]any{"status": store.RunFailed, "error": "boom"})
	e.publish("run-cancelled", map[string]any{"status": store.RunCancelled, "stopReason": "cancelled"})
	e.stop()

	d1 := e.diagnosis(t, "run-failed")
	if d1.RunID != "run-failed" || d1.SessionID != "s1" || d1.Status != store.RunFailed || d1.Error != "boom" {
		t.Fatalf("failed 诊断字段错误: %+v", d1)
	}
	d2 := e.diagnosis(t, "run-cancelled")
	if d2.RunID != "run-cancelled" || d2.SessionID != "s2" || d2.Status != store.RunCancelled {
		t.Fatalf("cancelled 诊断字段错误: %+v", d2)
	}
	for _, d := range []Diagnosis{d1, d2} {
		if d.CreatedAt.IsZero() {
			t.Fatal("CreatedAt 未填")
		}
		if d.Suggestion == "" {
			t.Fatal("Suggestion 未填")
		}
	}
	if len(e.all(t)) != 2 {
		t.Fatalf("应恰好 2 条诊断, got %d", len(e.all(t)))
	}
}

// 诊断必须带上租户/用户归属：否则 HTTP 层无法做隔离，只能全量暴露。
func TestAttachFillsTenantAndUserFromSession(t *testing.T) {
	e := newTestEnv(t, nil)
	if err := e.st.CreateSession(context.Background(), store.Session{
		ID: "s-owned", TenantID: "t1", UserID: "u1", Status: store.SessionActive,
	}); err != nil {
		t.Fatal(err)
	}
	e.makeRun(t, "s-owned", "run-owned", "boom")
	e.publish("run-owned", map[string]any{"status": store.RunFailed})
	e.stop()

	d := e.diagnosis(t, "run-owned")
	if d.TenantID != "t1" || d.UserID != "u1" {
		t.Fatalf("归属未从会话反查填入: %+v", d)
	}
}

// 会话读不到时诊断仍生成，但归属留空（宁可不出现在租户视图里，也不猜）。
func TestAttachMissingSessionLeavesOwnershipEmpty(t *testing.T) {
	e := newTestEnv(t, nil)
	// run 指向一个不存在的会话。
	e.makeRun(t, "s-ghost", "run-ghost", "boom")
	e.publish("run-ghost", map[string]any{"status": store.RunFailed})
	e.stop()

	d := e.diagnosis(t, "run-ghost")
	if d.TenantID != "" || d.UserID != "" {
		t.Fatalf("无会话时不该有归属: %+v", d)
	}
}

func TestAttachCompletedIgnored(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-ok", "")
	e.publish("run-ok", map[string]any{"status": store.RunCompleted, "stopReason": "end_turn"})
	e.stop()
	if got := e.all(t); len(got) != 0 {
		t.Fatalf("completed 不该生成诊断: %+v", got)
	}
}

func TestAttachNonTerminalIgnored(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-running", "")
	e.publish("run-running", map[string]any{"status": store.RunRunning})
	e.publish("run-running", map[string]any{"status": store.RunWaitingApproval})
	e.stop()
	if got := e.all(t); len(got) != 0 {
		t.Fatalf("非终态不该生成诊断: %+v", got)
	}
}

func TestAttachBadPayloadIgnored(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-bad", "")
	e.publishRaw("run-bad", json.RawMessage("{not json"))
	e.publishRaw("run-bad", nil)
	e.publishRaw("run-bad", json.RawMessage(`"a string"`))
	// 非 EventState 类型也不该触发。
	e.hub.Publish(store.Event{RunID: "run-bad", Type: store.EventError,
		Payload: json.RawMessage(`{"status":"failed"}`)})
	e.stop()
	if got := e.all(t); len(got) != 0 {
		t.Fatalf("坏 payload 不该生成诊断: %+v", got)
	}
}

func TestAttachDeduplicated(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-dup", "boom")
	for i := 0; i < 3; i++ {
		e.publish("run-dup", map[string]any{"status": store.RunFailed, "error": "boom"})
	}
	e.stop()
	if got := e.all(t); len(got) != 1 {
		t.Fatalf("同一 run 应只诊断一次, got %d 条: %+v", len(got), got)
	}
}

func TestAttachLastToolAndToolCount(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-tools", "boom")
	e.addTool(t, "run-tools", store.EventToolStart, "bash")
	e.addTool(t, "run-tools", store.EventToolUpdate, "bash")
	e.addTool(t, "run-tools", store.EventToolStart, "python3")
	// 非工具事件不能计入。
	e.addTool(t, "run-tools", store.EventMessageDelta, "IGNORED")
	e.publish("run-tools", map[string]any{"status": store.RunFailed})
	e.stop()

	d := e.diagnosis(t, "run-tools")
	if d.ToolCount != 3 {
		t.Fatalf("ToolCount = %d, want 3", d.ToolCount)
	}
	if d.LastTool != "python3" {
		t.Fatalf("LastTool = %q, want python3", d.LastTool)
	}
	if d.SessionID != "s1" || d.Error != "boom" {
		t.Fatalf("GetRun 补齐字段错误: %+v", d)
	}
}

func TestAttachErrorFromPayloadWhenRunErrorEmpty(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-payload", "")
	e.publish("run-payload", map[string]any{"status": store.RunFailed, "error": "command not found: ffmpeg"})
	e.stop()
	d := e.diagnosis(t, "run-payload")
	if d.Error != "command not found: ffmpeg" {
		t.Fatalf("payload.error 未补齐: %q", d.Error)
	}
}

func TestAttachNilActivityDoesNotPanic(t *testing.T) {
	e := newTestEnv(t, nil)
	e.makeRun(t, "s1", "run-nil", "boom")
	e.publish("run-nil", map[string]any{"status": store.RunFailed})
	e.stop()
	d := e.diagnosis(t, "run-nil")
	if d.Blocked != 0 || d.Failed != 0 {
		t.Fatalf("nil Activity 应给 0: Blocked=%d Failed=%d", d.Blocked, d.Failed)
	}
}

func TestAttachActivitySummary(t *testing.T) {
	e := newTestEnv(t, fixedActivity{blocked: 2, failed: 1})
	e.makeRun(t, "s1", "run-sum", "boom")
	e.publish("run-sum", map[string]any{"status": store.RunFailed})
	e.stop()
	d := e.diagnosis(t, "run-sum")
	if d.Blocked != 2 || d.Failed != 1 {
		t.Fatalf("活动摘要未写入: Blocked=%d Failed=%d", d.Blocked, d.Failed)
	}
}

func TestAttachSuggestionRules(t *testing.T) {
	cases := []struct {
		name    string
		errText string
		want    string
	}{
		{"command not found", "bash: command not found: ffmpeg", "沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
		{"not found in PATH", "foo not found in $PATH", "沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
		{"no such file", "no such file or directory", "沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
		{"大小写不敏感", "COMMAND NOT FOUND", "沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
		{"permission denied", "open /etc/x: permission denied", "被沙箱或审批拒绝；检查权限策略与出网白名单"},
		{"not permitted", "operation not permitted", "被沙箱或审批拒绝；检查权限策略与出网白名单"},
		{"Operation not permitted", "Operation not permitted", "被沙箱或审批拒绝；检查权限策略与出网白名单"},
		{"egress", "egress blocked for api.example.com", "出网被策略拦截；在 /v1/admin/egress/policy 加域名白名单或提交审批"},
		{"出网被拒", "出网被拒", "出网被策略拦截；在 /v1/admin/egress/policy 加域名白名单或提交审批"},
		{"deny", "policy deny", "出网被策略拦截；在 /v1/admin/egress/policy 加域名白名单或提交审批"},
		{"timeout", "execution timeout", "执行超时；拆小任务或调大超时"},
		{"deadline", "deadline exceeded", "执行超时；拆小任务或调大超时"},
		{"context deadline exceeded", "rpc error: context deadline exceeded", "执行超时；拆小任务或调大超时"},
		{"401", "HTTP 401", "凭据无效或过期；重新登录或检查会话级沙箱凭据"},
		{"403", "403 forbidden", "凭据无效或过期；重新登录或检查会话级沙箱凭据"},
		{"unauthorized", "Unauthorized access", "凭据无效或过期；重新登录或检查会话级沙箱凭据"},
		{"凭据", "凭据已过期", "凭据无效或过期；重新登录或检查会话级沙箱凭据"},
		{"LLM", "LLM gateway error", "模型上游不可用；检查网关 key 与上游地址"},
		{"model", "model not available", "模型上游不可用；检查网关 key 与上游地址"},
		{"upstream", "upstream connect failed", "模型上游不可用；检查网关 key 与上游地址"},
		{"502", "502 bad gateway", "模型上游不可用；检查网关 key 与上游地址"},
		{"规则优先级", "command not found: model", "沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
		{"兜底", "some random failure", fallbackSuggestion},
	}

	e := newTestEnv(t, nil)
	for i, c := range cases {
		rid := fmt.Sprintf("case-%02d", i)
		e.makeRun(t, "s1", rid, c.errText)
		e.publish(rid, map[string]any{"status": store.RunFailed, "error": c.errText})
	}
	e.stop()
	for i, c := range cases {
		rid := fmt.Sprintf("case-%02d", i)
		d := e.diagnosis(t, rid)
		want := c.want
		if want == fallbackSuggestion {
			// 兜底文案含 {id}，落库前会替换成真实 runID。
			want = strings.ReplaceAll(fallbackSuggestion, "{id}", rid)
		}
		if d.Suggestion != want {
			t.Errorf("%s (err=%q):\n got=%q\nwant=%q", c.name, c.errText, d.Suggestion, want)
		}
	}
}

// 兜底建议里的 {id} 占位符必须替换成真实 runID 再落库，不能留一条点不开的假路径。
func TestAttachFallbackSuggestionReplacesID(t *testing.T) {
	e := newTestEnv(t, nil)
	runID := "run-fallback-xyz"
	e.makeRun(t, "s1", runID, "some random failure")
	e.publish(runID, map[string]any{"status": store.RunFailed, "error": "some random failure"})
	e.stop()

	d := e.diagnosis(t, runID)
	if strings.Contains(d.Suggestion, "{id}") {
		t.Fatalf("建议里仍残留 {id} 占位符: %q", d.Suggestion)
	}
	if !strings.Contains(d.Suggestion, "/v1/runs/"+runID+"/activity") {
		t.Fatalf("建议里没有真实 runID: %q", d.Suggestion)
	}
	if d.Suggestion == fallbackSuggestion {
		t.Fatal("兜底建议没有被替换")
	}
}

func TestAttachStopWaitsForInflight(t *testing.T) {
	act := &blockingActivity{entered: make(chan struct{}), release: make(chan struct{})}
	e := newTestEnv(t, act)
	e.makeRun(t, "s1", "run-slow", "boom")
	e.publish("run-slow", map[string]any{"status": store.RunFailed})

	select {
	case <-act.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("诊断 goroutine 未进入慢 Activity")
	}

	// stop 必须主动 cancel 在飞诊断（慢 Activity 尊重 ctx），并等它退出。
	done := make(chan struct{})
	go func() { e.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop 未等在飞 goroutine 退出（疑似泄漏 / 傻等 30s）")
	}

	// stop 之后不再处理新事件。
	e.publish("run-after", map[string]any{"status": store.RunFailed})
	if got := e.all(t); len(got) != 1 || got[0].RunID != "run-slow" {
		t.Fatalf("stop 后仍生成了诊断或丢了在飞那条: %+v", got)
	}
}

func TestAttachStopWaitsForSlowFiniteActivity(t *testing.T) {
	e := newTestEnv(t, sleepActivity(80*time.Millisecond))
	e.makeRun(t, "s1", "run-sleep", "boom")
	e.publish("run-sleep", map[string]any{"status": store.RunFailed})

	start := time.Now()
	e.stop()
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("stop 没有等在飞诊断收工")
	}
	// wg.Wait 收敛后诊断必然已落库。
	e.diagnosis(t, "run-sleep")
}

func TestAttachWriteFailureOnlyLogs(t *testing.T) {
	e := newTestEnv(t, nil)
	// 把目标文件换成一个目录，让 append 的 rename 必然失败。
	_ = os.Remove(filepath.Join(e.dir, insightsFileName))
	if err := os.Mkdir(filepath.Join(e.dir, insightsFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	e.makeRun(t, "s1", "run-x", "boom")
	e.publish("run-x", map[string]any{"status": store.RunFailed})
	e.stop() // 写库失败只记日志：不 panic、不卡死
}

func TestAttachNilHubOrStoreIsNoop(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	// 两者都应返回可调用的 stop，且不会 panic。
	stop1 := Attach(nil, &fakeStore{}, nil, t.TempDir(), lg)
	stop1()
	stop2 := Attach(eventbus.New(), nil, nil, t.TempDir(), lg)
	stop2()
}

// fakeStore 只为满足 Attach 的非空 store.Store 参数；本用例不会真正触发订阅回调。
type fakeStore struct{}

func (fakeStore) CreateSession(context.Context, store.Session) error { return nil }
func (fakeStore) GetSession(context.Context, string) (store.Session, error) {
	return store.Session{}, nil
}
func (fakeStore) ListSessions(context.Context) ([]store.Session, error) { return nil, nil }
func (fakeStore) PutSession(context.Context, store.Session) error       { return nil }
func (fakeStore) DeleteSession(context.Context, string) error           { return nil }
func (fakeStore) CreateRun(context.Context, store.Run) error            { return nil }
func (fakeStore) GetRun(context.Context, string) (store.Run, error)     { return store.Run{}, nil }
func (fakeStore) ListRuns(context.Context, string) ([]store.Run, error) { return nil, nil }
func (fakeStore) UpdateRun(context.Context, store.Run) error            { return nil }
func (fakeStore) Append(context.Context, string, store.NewEvent) (store.Event, error) {
	return store.Event{}, nil
}
func (fakeStore) Events(context.Context, string, int64, int) ([]store.Event, error) {
	return nil, nil
}
func (fakeStore) Close() error { return nil }

var _ store.Store = fakeStore{}
