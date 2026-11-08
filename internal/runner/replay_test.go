package runner

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/insight"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// replayHarness 用真实的 store.OpenFile + eventbus + insight.Attach/Open，
// 只把 Manager 用裸结构体装配（不需要沙箱 provider）：回灌逻辑全在
// replayContext 里，StartRun 只是调用它。这样能精确验证「诊断真的落库、
// Pending 真的读到、MarkConsumed 真的生效」。
type replayHarness struct {
	st   store.Store
	ins  insight.Store
	hub  *eventbus.Hub
	m    *Manager
	stop func()
}

func newReplayHarness(t *testing.T) *replayHarness {
	t.Helper()
	root := t.TempDir()
	st, err := store.OpenFile(filepath.Join(root, "store"), store.Options{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	hub := eventbus.New()
	lg := log.New(io.Discard, "", 0)
	insDir := filepath.Join(root, "insights")
	// Attach 与 Open 共用 dataDir → 同一个 fileStore，诊断写完立刻可见。
	stop := insight.Attach(hub, st, nil, insDir, lg)
	ins, err := insight.Open(insDir)
	if err != nil {
		t.Fatal(err)
	}
	hints := NewRuntimeHints()
	hints.Set(ins, "", "")
	m := &Manager{cfg: Config{Store: st, Hub: hub, Hints: hints, InsightReplay: true}, log: lg}
	t.Cleanup(func() {
		stop()
		_ = st.Close()
	})
	return &replayHarness{st: st, ins: ins, hub: hub, m: m, stop: stop}
}

// seedFailedRun 造一条失败 run 并发布终态，让 insight.Attach 异步生成诊断。
func (h *replayHarness) seedFailedRun(t *testing.T, sessionID, runID string) {
	t.Helper()
	r := store.Run{
		ID: runID, SessionID: sessionID, Status: store.RunFailed,
		Error: "command not found: foo", CreatedAt: time.Now().UTC(),
		EndedAt: time.Now().UTC(), NextSeq: 1,
	}
	if err := h.st.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(map[string]any{"status": store.RunFailed, "error": "command not found: foo"})
	h.hub.Publish(store.Event{RunID: runID, Type: store.EventState, Payload: p})
}

// waitUnconsumed 轮询直到至少有一条未消费诊断落库（diagnose 在独立 goroutine）。
func (h *replayHarness) waitUnconsumed(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ds, err := h.ins.Pending(context.Background(), sessionID, time.Time{}, 10)
		if err == nil && len(ds) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("诊断未生成")
}

func (h *replayHarness) newSession(t *testing.T, id string, createdAt time.Time) store.Session {
	t.Helper()
	sess := store.Session{
		ID: id, Status: store.SessionActive,
		CreatedAt: createdAt, LastActiveAt: createdAt,
	}
	if err := h.st.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// 核心回归：同一条诊断只注入一次；用户成功后不再注入（即便仍有未消费诊断）。
func TestReplayInjectsOnceAndStopsAfterSuccess(t *testing.T) {
	h := newReplayHarness(t)
	ctx := context.Background()
	sess := h.newSession(t, "sess-1", time.Now().UTC().Add(-time.Minute))

	// 1) 造一条失败诊断（CreatedAt ≈ now，晚于会话创建时间）。
	h.seedFailedRun(t, sess.ID, "run-fail-1")
	h.waitUnconsumed(t, sess.ID)

	// 2) 无成功记录：since = 会话创建时间 → 命中 → 注入 + MarkConsumed。
	got1 := h.m.replayContext(ctx, sess, "做任务 A")
	if !strings.Contains(got1, "[上轮失败上下文]") {
		t.Fatalf("第 1 轮应注入失败上下文: %q", got1)
	}
	if !strings.Contains(got1, "command not found: foo") || !strings.Contains(got1, "做任务 A") {
		t.Fatalf("注入内容缺错误文本或用户指令: %q", got1)
	}

	// 3) 第 2 轮：诊断已消费 → 不再注入。
	got2 := h.m.replayContext(ctx, sess, "做任务 B")
	if strings.Contains(got2, "[上轮失败上下文]") {
		t.Fatalf("同一条诊断被重复注入: %q", got2)
	}
	if got2 != "做任务 B" {
		t.Fatalf("无诊断时应原样返回用户输入, got %q", got2)
	}

	// 4) 成功后不再注入：造一个 EndedAt 明显晚于任何「现在」的成功 run，
	//    再放一条「未消费」的失败诊断。since 取该成功 run 的 EndedAt，
	//    旧诊断应被时间闸门挡掉——这正是「用户已成功也照注入」的修复点。
	successAt := time.Now().UTC().Add(time.Hour)
	ok := store.Run{
		ID: "run-ok", SessionID: sess.ID, Status: store.RunCompleted,
		CreatedAt: successAt, EndedAt: successAt, NextSeq: 1,
	}
	if err := h.st.CreateRun(ctx, ok); err != nil {
		t.Fatal(err)
	}
	h.seedFailedRun(t, sess.ID, "run-fail-2")
	h.waitUnconsumed(t, sess.ID) // 确认确有一条未消费诊断在场

	got3 := h.m.replayContext(ctx, sess, "做任务 C")
	if strings.Contains(got3, "[上轮失败上下文]") {
		t.Fatalf("用户已成功后仍注入了成功前的诊断: %q", got3)
	}
	if got3 != "做任务 C" {
		t.Fatalf("成功闸门未生效但仍改写了输入, got %q", got3)
	}
}

// INSIGHT_REPLAY 关闭时原样返回（既有可关语义不变）。
func TestReplayDisabledByConfig(t *testing.T) {
	h := newReplayHarness(t)
	ctx := context.Background()
	sess := h.newSession(t, "sess-1", time.Now().UTC().Add(-time.Minute))
	h.seedFailedRun(t, sess.ID, "run-fail")
	h.waitUnconsumed(t, sess.ID)

	h.m.cfg.InsightReplay = false
	if got := h.m.replayContext(ctx, sess, "原样指令"); got != "原样指令" {
		t.Fatalf("INSIGHT_REPLAY 关闭时不应注入, got %q", got)
	}
}

// 诊断写入失败 / 标记失败只记日志、不 panic、不改写行为（旁路容错）。
func TestReplayToleratesNilInsightStore(t *testing.T) {
	h := newReplayHarness(t)
	h.m.cfg.Hints = NewRuntimeHints() // insights 为 nil
	sess := h.newSession(t, "sess-1", time.Now().UTC())
	if got := h.m.replayContext(context.Background(), sess, "照常跑"); got != "照常跑" {
		t.Fatalf("insight store 为 nil 时应原样返回, got %q", got)
	}
}
