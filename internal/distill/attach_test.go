package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// Attach 的测试走真实 store.OpenFile（newHarness 提供）+ 真实事件流，
// 只是把 Distiller 换成可观察的假实现：要断言的是「哪些 run 被触发」，
// 而不是草稿正文（那部分由 distiller_test.go 覆盖）。

// syncBuffer 让 log.Logger 的写与测试断言串行：bytes.Buffer 本身不是
// 并发安全的，而自动沉淀的日志来自后台 goroutine。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeDistiller struct {
	mu     sync.Mutex
	calls  []Request
	notify chan Request

	enter   chan struct{} // Distill 进入时通知（阻塞型测试用）
	release chan struct{} // 释放阻塞
	err     error
}

func newFakeDistiller() *fakeDistiller {
	return &fakeDistiller{notify: make(chan Request, 64)}
}

func (f *fakeDistiller) Enabled() bool { return true }

func (f *fakeDistiller) Distill(_ context.Context, req Request) (Draft, error) {
	if f.enter != nil {
		select {
		case f.enter <- struct{}{}:
		default:
		}
		<-f.release
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	select {
	case f.notify <- req:
	default:
	}
	return Draft{}, f.err
}

func (f *fakeDistiller) List(context.Context, string) ([]Draft, error) { return nil, nil }
func (f *fakeDistiller) Accept(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeDistiller) Reject(context.Context, string, string) error { return nil }

func (f *fakeDistiller) wait(t *testing.T, d time.Duration) (Request, bool) {
	t.Helper()
	select {
	case r := <-f.notify:
		return r, true
	case <-time.After(d):
		return Request{}, false
	}
}

func (f *fakeDistiller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDistiller) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if n := f.count(); n != 0 {
		t.Fatalf("不应触发 Distill，实际调用 %d 次", n)
	}
}

type attachHarness struct {
	h    *harness
	d    *fakeDistiller
	bus  *eventbus.Hub
	stop func()
	lg   *log.Logger
	buf  *syncBuffer
}

func newAttachHarness(t *testing.T) *attachHarness {
	t.Helper()
	// 显式打开：宿主环境里万一设了 DISTILL_AUTO=false 也不该让测试变绿。
	t.Setenv("DISTILL_AUTO", "true")
	h := newHarness(t, 40)
	d := newFakeDistiller()
	buf := &syncBuffer{}
	lg := log.New(buf, "", 0)
	bus := eventbus.New()
	stop := Attach(bus, d, h.st, lg)
	a := &attachHarness{h: h, d: d, bus: bus, stop: stop, lg: lg, buf: buf}
	t.Cleanup(stop)
	return a
}

func publishState(bus *eventbus.Hub, runID, status string) {
	p, _ := json.Marshal(map[string]any{"status": status})
	bus.Publish(store.Event{RunID: runID, Type: store.EventState, Payload: p})
}

// twoOKRun 造一条有 2 次成功工具调用的终态 run。
func twoOKRun(a *attachHarness) store.Run {
	return a.h.newRun("build and write note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "note.md"}),
		toolEnd("t2", "completed"),
	)
}

// ── 1. 终态触发 ──────────────────────────────────────────────

// 自动沉淀「只在 status == completed」时触发（不再是任意 IsTerminal）。
func TestAttachTriggersOnCompletedOnly(t *testing.T) {
	t.Run("completed triggers", func(t *testing.T) {
		a := newAttachHarness(t)
		run := twoOKRun(a)
		publishState(a.bus, run.ID, store.RunCompleted)
		req, ok := a.d.wait(t, 3*time.Second)
		if !ok {
			t.Fatal("completed 应触发自动沉淀")
		}
		if req.RunID != run.ID {
			t.Errorf("RunID = %q，期望 %q", req.RunID, run.ID)
		}
		// 归属是从 store 反查的（事件里没有 tenant/user）。
		if req.TenantID != "t1" || req.UserID != "u1" || req.SessionID != "sess-1" {
			t.Errorf("归属反查不对: %+v", req)
		}
	})

	// failed / cancelled 也是终态，但不是成功轨迹，不该刷草稿。
	for _, status := range []string{store.RunFailed, store.RunCancelled} {
		t.Run(status+" does not trigger", func(t *testing.T) {
			a := newAttachHarness(t)
			run := twoOKRun(a)
			publishState(a.bus, run.ID, status)
			a.d.expectNone(t, 300*time.Millisecond)
		})
	}
}

// ── 2. 非终态 / 非状态事件不触发 ──────────────────────────────

func TestAttachIgnoresNonTerminalAndOtherEvents(t *testing.T) {
	a := newAttachHarness(t)
	run := twoOKRun(a)
	for _, status := range []string{store.RunQueued, store.RunRunning, store.RunWaitingApproval, "", "bogus"} {
		publishState(a.bus, run.ID, status)
	}
	// 其它事件类型即使 payload 长得像终态也不算。
	p, _ := json.Marshal(map[string]any{"status": store.RunCompleted})
	a.bus.Publish(store.Event{RunID: run.ID, Type: store.EventToolUpdate, Payload: p})
	a.bus.Publish(store.Event{RunID: "", Type: store.EventState, Payload: p})
	a.d.expectNone(t, 300*time.Millisecond)
}

func TestAttachBadPayloadDoesNotTrigger(t *testing.T) {
	a := newAttachHarness(t)
	run := twoOKRun(a)
	a.bus.Publish(store.Event{RunID: run.ID, Type: store.EventState, Payload: json.RawMessage(`{not json`)})
	a.bus.Publish(store.Event{RunID: run.ID, Type: store.EventState})
	a.d.expectNone(t, 300*time.Millisecond)
}

// ── 3. 去重 ──────────────────────────────────────────────────

func TestAttachDeduplicatesPerRun(t *testing.T) {
	a := newAttachHarness(t)
	run := twoOKRun(a)
	for i := 0; i < 5; i++ {
		publishState(a.bus, run.ID, store.RunCompleted)
	}
	if _, ok := a.d.wait(t, 3*time.Second); !ok {
		t.Fatal("第一次终态没触发")
	}
	time.Sleep(300 * time.Millisecond)
	if n := a.d.count(); n != 1 {
		t.Fatalf("同一 run 沉淀了 %d 次，期望 1", n)
	}

	// 不同 run 互不干扰。
	run2 := twoOKRun(a)
	publishState(a.bus, run2.ID, store.RunCompleted)
	if _, ok := a.d.wait(t, 3*time.Second); !ok {
		t.Fatal("第二个 run 没触发")
	}
	if n := a.d.count(); n != 2 {
		t.Fatalf("两个 run 共沉淀 %d 次，期望 2", n)
	}
}

// ── 4. 少于 2 次成功工具调用不触发 ────────────────────────────

func TestAttachSkipsRunsWithTooFewSuccessfulSteps(t *testing.T) {
	a := newAttachHarness(t)

	zero := a.h.newRun("纯问答", store.RunCompleted)
	one := a.h.newRun("单步", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "ls"}),
		toolEnd("t1", "completed"),
	)
	mixed := a.h.newRun("一成一败", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "make"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "x"}),
		toolEnd("t2", "failed"),
	)
	for _, r := range []store.Run{zero, one, mixed} {
		publishState(a.bus, r.ID, store.RunCompleted)
	}
	// 少于 2 步要静默跳过，别刷错误日志。
	a.d.expectNone(t, 300*time.Millisecond)
	if a.buf.String() != "" {
		t.Errorf("步数不足时产生了日志（应当静默跳过）: %s", a.buf.String())
	}
}

// ── 5. 开关 / 空依赖 ─────────────────────────────────────────

func TestAttachDisabledByEnv(t *testing.T) {
	t.Setenv("DISTILL_AUTO", "false")
	h := newHarness(t, 40)
	d := newFakeDistiller()
	bus := eventbus.New()
	stop := Attach(bus, d, h.st, log.New(io.Discard, "", 0))
	defer stop()
	run := h.newRun("build and note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "a"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "b"}),
		toolEnd("t2", "completed"),
	)
	publishState(bus, run.ID, store.RunCompleted)
	d.expectNone(t, 300*time.Millisecond)
}

func TestAttachNoopOnMissingDepsOrDisabledDistiller(t *testing.T) {
	t.Setenv("DISTILL_AUTO", "true")
	h := newHarness(t, 40)
	d := newFakeDistiller()
	bus := eventbus.New()
	lg := log.New(io.Discard, "", 0)
	// nil hub / nil store / nil distiller 都应当是 noop，不 panic。
	for _, stop := range []func(){
		Attach(nil, d, h.st, lg),
		Attach(bus, nil, h.st, lg),
		Attach(bus, d, nil, lg),
	} {
		stop()
	}
	// 关掉的沉淀器不该订阅（否则每条终态刷 ErrDisabled）。
	disabled := &distiller{} // Enabled() == false
	stop := Attach(bus, disabled, h.st, lg)
	defer stop()
	run := twoOKRun(&attachHarness{h: h})
	publishState(bus, run.ID, store.RunCompleted)
	d.expectNone(t, 200*time.Millisecond)
}

// ── 6. Distill 报错只记日志 ──────────────────────────────────

func TestAttachDistillErrorOnlyLogs(t *testing.T) {
	a := newAttachHarness(t)
	a.d.err = errors.New("模拟写草稿失败")
	run := twoOKRun(a)
	publishState(a.bus, run.ID, store.RunCompleted)
	if _, ok := a.d.wait(t, 3*time.Second); !ok {
		t.Fatal("没触发 Distill")
	}
	// 等日志落地（Distill 返回后才 Printf）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(a.buf.String(), "模拟写草稿失败") {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(a.buf.String(), "模拟写草稿失败") {
		t.Fatalf("Distill 报错没有记日志: %q", a.buf.String())
	}
	// 不 panic、stop 能正常返回。
	a.stop()
}

// ── 7. stop 等在飞 goroutine 退出 ────────────────────────────

func TestAttachStopWaitsForInFlightDistill(t *testing.T) {
	a := newAttachHarness(t)
	a.d.enter = make(chan struct{}, 1)
	a.d.release = make(chan struct{})
	run := twoOKRun(a)
	publishState(a.bus, run.ID, store.RunCompleted)

	select {
	case <-a.d.enter:
	case <-time.After(3 * time.Second):
		t.Fatal("Distill 没进入")
	}

	// stop 必须阻塞到 Distill 返回，不能提前放走 goroutine。
	done := make(chan struct{})
	go func() { a.stop(); close(done) }()
	select {
	case <-done:
		t.Fatal("stop 在 Distill 还没结束时就返回了（goroutine 泄漏）")
	case <-time.After(150 * time.Millisecond):
	}
	close(a.d.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("release 之后 stop 仍未返回")
	}
	// 重复调用安全。
	a.stop()
}

// ── 8. 回调本身不能 panic / 不能阻塞Publish ───────────────────

func TestAttachEventCallbackIsFastAndSafe(t *testing.T) {
	a := newAttachHarness(t)
	run := twoOKRun(a)
	start := time.Now()
	for i := 0; i < 200; i++ {
		publishState(a.bus, run.ID, store.RunCompleted)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("200 次 Publish 耗时 %v，回调里做了重活", elapsed)
	}
	if _, ok := a.d.wait(t, 3*time.Second); !ok {
		t.Fatal("没触发 Distill")
	}
	a.stop()
}

// ── 9. DISTILL_AUTO 的各种写法 ───────────────────────────────

// 只有 false/0/no/off（大小写不敏感、去空白）为关，其余一律为开（默认开）。
func TestDistillAutoEnabledParsing(t *testing.T) {
	off := []string{"false", "FALSE", "False", "0", "no", "NO", "off", "OFF", "  false  ", " off "}
	on := []string{"", "true", "TRUE", "1", "yes", "on", "garbage", "falseish", "-1"}
	for _, v := range off {
		t.Run("off="+v, func(t *testing.T) {
			t.Setenv("DISTILL_AUTO", v)
			if distillAutoEnabled() {
				t.Fatalf("DISTILL_AUTO=%q 应为关", v)
			}
		})
	}
	for _, v := range on {
		t.Run("on="+v, func(t *testing.T) {
			t.Setenv("DISTILL_AUTO", v)
			if !distillAutoEnabled() {
				t.Fatalf("DISTILL_AUTO=%q 应为开", v)
			}
		})
	}
}

// runAttachWithEnv 用真实 store + eventbus 起一次 Attach，返回假 Distiller 供计数。
func runAttachWithEnv(t *testing.T, env string) *fakeDistiller {
	t.Helper()
	t.Setenv("DISTILL_AUTO", env)
	h := newHarness(t, 40)
	d := newFakeDistiller()
	bus := eventbus.New()
	stop := Attach(bus, d, h.st, log.New(io.Discard, "", 0))
	t.Cleanup(stop)
	run := twoOKRun(&attachHarness{h: h})
	publishState(bus, run.ID, store.RunCompleted)
	return d
}

// Attach 真的认这些「关」写法（不只是纯函数自测）。
func TestAttachDisabledByCommonOffValues(t *testing.T) {
	for _, v := range []string{"0", "no", "OFF", "  false  "} {
		t.Run(v, func(t *testing.T) {
			d := runAttachWithEnv(t, v)
			d.expectNone(t, 300*time.Millisecond)
		})
	}
}

// 非「关」写法（含未设置 / 空串 / 任意值）都保持开启。
func TestAttachEnabledByNonOffValues(t *testing.T) {
	for _, v := range []string{"", "1", "yes", "on", "garbage"} {
		t.Run("value="+v, func(t *testing.T) {
			d := runAttachWithEnv(t, v)
			if _, ok := d.wait(t, 3*time.Second); !ok {
				t.Fatalf("DISTILL_AUTO=%q 应开启并沉淀", v)
			}
		})
	}
}
