package memory

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// fakeStore 记录被写入的记忆。Ingest 是自动入库唯一的副作用点。
type fakeStore struct {
	mu    sync.Mutex
	items []Item
	// ch 用来让测试不必 sleep 轮询
	ch chan Item
}

func newFakeStore() *fakeStore { return &fakeStore{ch: make(chan Item, 8)} }

func (f *fakeStore) Ingest(_ context.Context, it Item) (Item, error) {
	f.mu.Lock()
	f.items = append(f.items, it)
	f.mu.Unlock()
	select {
	case f.ch <- it:
	default:
	}
	return it, nil
}

func (f *fakeStore) Search(context.Context, Query) ([]Item, error) { return nil, nil }
func (f *fakeStore) Delete(context.Context, string, string, string) error {
	return nil
}
func (f *fakeStore) Stats(context.Context) (Stats, error) { return Stats{Enabled: true}, nil }
func (f *fakeStore) Close() error                         { return nil }

func (f *fakeStore) snapshot() []Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Item(nil), f.items...)
}

type fakeRuns struct {
	run   store.Run
	sess  store.Session
	delay time.Duration
}

func (f *fakeRuns) GetRun(context.Context, string) (store.Run, error) { return f.run, nil }
func (f *fakeRuns) GetSession(context.Context, string) (store.Session, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.sess, nil
}

func quietLogger() *log.Logger { return log.New(discard{}, "", 0) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func publish(hub *eventbus.Hub, runID, typ string, payload any) {
	raw, _ := json.Marshal(payload)
	hub.Publish(store.Event{RunID: runID, Type: typ, Payload: raw})
}

func waitItem(t *testing.T, st *fakeStore, timeout time.Duration) Item {
	t.Helper()
	select {
	case it := <-st.ch:
		return it
	case <-time.After(timeout):
		t.Fatal("等待自动入库超时")
		return Item{}
	}
}

func TestAttachWritesConversationSummary(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "帮我看看这个季度的预算", Status: store.RunCompleted},
		sess: store.Session{ID: "sess-1", TenantID: "tenant-a", UserID: "user-1"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunRunning})
	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": "好的，"})
	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": "预算还剩 12 万。"})
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})

	it := waitItem(t, st, 3*time.Second)
	if it.Kind != KindConversation {
		t.Fatalf("kind = %s, want %s", it.Kind, KindConversation)
	}
	if it.TenantID != "tenant-a" || it.UserID != "user-1" || it.SessionID != "sess-1" {
		t.Fatalf("归属字段错误: %+v", it)
	}
	want := "用户: 帮我看看这个季度的预算\n助手: 好的，预算还剩 12 万。"
	if it.Text != want {
		t.Fatalf("text = %q\nwant = %q", it.Text, want)
	}
	if it.Meta["run_id"] != "run-1" || it.Meta["status"] != store.RunCompleted {
		t.Fatalf("meta = %v", it.Meta)
	}
}

func TestAttachIsIdempotentPerRun(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "hi"},
		sess: store.Session{ID: "sess-1", TenantID: "t", UserID: "u"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": "回复"})
	for i := 0; i < 3; i++ {
		publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})
	}
	waitItem(t, st, 3*time.Second)
	// 重复终态事件不产生第二条记忆；给后台 goroutine 一点时间落第二条（如果有 bug）
	time.Sleep(150 * time.Millisecond)
	if n := len(st.snapshot()); n != 1 {
		t.Fatalf("同一 run 写了 %d 条记忆，want 1", n)
	}
}

func TestAttachIgnoresFailedAndNonTerminal(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "hi"},
		sess: store.Session{ID: "sess-1", TenantID: "t", UserID: "u"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": "x"})
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunFailed})
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunWaitingApproval})
	time.Sleep(150 * time.Millisecond)
	if n := len(st.snapshot()); n != 0 {
		t.Fatalf("failed/waiting 不应入库，实际写了 %d 条", n)
	}
}

func TestAttachWritesOnCancelled(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "长任务"},
		sess: store.Session{ID: "sess-1", TenantID: "t", UserID: "u"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": "做到一半"})
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCancelled})

	it := waitItem(t, st, 3*time.Second)
	if it.Meta["status"] != store.RunCancelled {
		t.Fatalf("status = %v", it.Meta["status"])
	}
}

func TestAttachTruncatesLongSides(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	longIn := strings.Repeat("问", autoTextLimit*2)
	longOut := strings.Repeat("答", autoTextLimit*2)
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: longIn},
		sess: store.Session{ID: "sess-1", TenantID: "t", UserID: "u"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventMessageDelta, map[string]any{"text": longOut})
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})

	it := waitItem(t, st, 3*time.Second)
	userPart, assistantPart, ok := strings.Cut(it.Text, "\n助手: ")
	if !ok {
		t.Fatalf("text 格式不符: %q", it.Text)
	}
	userPart = strings.TrimPrefix(userPart, "用户: ")
	for _, part := range []string{userPart, assistantPart} {
		if n := len([]rune(part)); n > autoTextLimit+1 { // +1 是省略号
			t.Fatalf("单侧长度 %d 超过上限 %d", n, autoTextLimit)
		}
	}
}

func TestAttachSkipsSessionWithoutOwner(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "hi"},
		sess: store.Session{ID: "sess-1"}, // 单租户兼容模式，没有归属
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	defer stop()

	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})
	time.Sleep(200 * time.Millisecond)
	if n := len(st.snapshot()); n != 0 {
		t.Fatalf("无归属会话不应写入（会变成谁都能查的记忆），实际 %d 条", n)
	}
}

// 记忆能力关着时不能留下后台协程，也不能订阅 hub。
func TestAttachDisabledStoreDoesNothing(t *testing.T) {
	hub := eventbus.New()
	stop := AttachWith(Disabled(), hub, &fakeRuns{}, quietLogger())
	if stop == nil {
		t.Fatal("stop 不能是 nil")
	}
	stop() // 可安全调用
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})
}

func TestAttachWithoutRunsIsNoop(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	stop := Attach(st, hub, quietLogger())
	defer stop()
	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})
	time.Sleep(150 * time.Millisecond)
	if n := len(st.snapshot()); n != 0 {
		t.Fatalf("没有归属查询时不应写入，实际 %d 条", n)
	}
}

func TestAttachStopStopsIngest(t *testing.T) {
	st := newFakeStore()
	hub := eventbus.New()
	runs := &fakeRuns{
		run:  store.Run{ID: "run-1", SessionID: "sess-1", Input: "hi"},
		sess: store.Session{ID: "sess-1", TenantID: "t", UserID: "u"},
	}
	stop := AttachWith(st, hub, runs, quietLogger())
	stop()

	publish(hub, "run-1", store.EventState, map[string]any{"status": store.RunCompleted})
	time.Sleep(150 * time.Millisecond)
	if n := len(st.snapshot()); n != 0 {
		t.Fatalf("stop 之后仍在写入（%d 条）", n)
	}
}
