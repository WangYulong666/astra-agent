package remotedesk

import (
	"context"
	"errors"
	"testing"
	"time"
)

func activeSession(mode string) Session {
	return Session{ID: "sess_test", Mode: mode, Status: StatusActive}
}

func testPolicy() Policy {
	return Policy{
		MaxFPS:       30,
		MaxWidth:     320,
		Quality:      60,
		MaxInputRate: 200,
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// 静止画面不该反复发帧：同一图案抓十几次只应发出 1 帧（心跳还没到）。
func TestStreamSkipsStaticFrames(t *testing.T) {
	cap := NewFakeCapture(1)
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), cap, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	time.Sleep(300 * time.Millisecond)

	grabs := cap.GrabCount()
	frames := rt.Stats().Frames
	if grabs < 5 {
		t.Fatalf("抓屏泵跑得太少（%d 次），测不出跳帧", grabs)
	}
	if frames < 1 || frames > 2 {
		t.Fatalf("静止画面在 %d 次抓屏后只该发 1~2 帧，实际 %d 帧", grabs, frames)
	}
}

// 画面在变就每帧都发。
func TestStreamSendsChangingFrames(t *testing.T) {
	cap := NewFakeCapture(8)
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), cap, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	time.Sleep(300 * time.Millisecond)
	st := rt.Stats()
	if st.Frames < 3 {
		t.Fatalf("画面持续变化时应连续发帧，实际只发了 %d 帧（抓了 %d 次）", st.Frames, cap.GrabCount())
	}
}

// 心跳：静止画面也至少每 2 秒一帧，否则手机端会以为断了。
func TestStreamHeartbeat(t *testing.T) {
	cap := NewFakeCapture(1)
	pol := testPolicy()
	pol.MaxFPS = 20
	rt, err := newRuntime(activeSession(ModeView), pol, cap, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	time.Sleep(2300 * time.Millisecond)
	if st := rt.Stats(); st.Frames < 2 {
		t.Fatalf("2.3 秒内静止画面应至少 2 帧（首帧+心跳），实际 %d", st.Frames)
	}
}

// 背压：订阅者不读，Dropped 增长，但抓屏泵照跑不误。
func TestStreamBackpressure(t *testing.T) {
	cap := NewFakeCapture(64)
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), cap, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	ch, err := rt.Frames(context.Background())
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	_ = ch // 故意不读

	time.Sleep(400 * time.Millisecond)

	st := rt.Stats()
	if st.Dropped == 0 {
		t.Fatalf("订阅者不读时应产生丢帧，实际 dropped=0（frames=%d）", st.Frames)
	}
	if st.Frames < 5 {
		t.Fatalf("抓屏泵被慢客户端拖住了：400ms 只发了 %d 帧", st.Frames)
	}

	closed := make(chan struct{})
	go func() { _ = rt.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 被阻塞")
	}
}

// 限速：1 秒内 300 个事件，真正落到注入后端的不能超过 MaxInputRate。
func TestStreamRateLimit(t *testing.T) {
	pol := testPolicy()
	pol.MaxFPS = 1 // 少抓点屏，把时间留给输入
	pol.MaxInputRate = 10
	in := NewFakeInput()
	rt, err := newRuntime(activeSession(ModeControl), pol, NewFakeCapture(1), in, nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	denied := 0
	for i := 0; i < 300; i++ {
		err := rt.Input(InputEvent{Op: "move", X: 0.5, Y: 0.5})
		if err != nil {
			if !errors.Is(err, ErrDenied) {
				t.Fatalf("超速应返回 ErrDenied，实际 %v", err)
			}
			denied++
		}
	}
	if got := in.EventCount(); got > pol.MaxInputRate {
		t.Fatalf("注入后端收到 %d 个事件，超过上限 %d", got, pol.MaxInputRate)
	}
	if got := in.EventCount(); got != pol.MaxInputRate {
		t.Fatalf("限制内的事件应全部放行，实际放行 %d / %d", got, pol.MaxInputRate)
	}
	if denied != 300-pol.MaxInputRate {
		t.Fatalf("应拒绝 %d 个，实际 %d", 300-pol.MaxInputRate, denied)
	}
	if st := rt.Stats(); st.Inputs != uint64(pol.MaxInputRate) {
		t.Fatalf("Stats.Inputs 应为 %d，实际 %d", pol.MaxInputRate, st.Inputs)
	}
}

// view 模式不许注入；control 才行。
func TestStreamInputModeGate(t *testing.T) {
	in := NewFakeInput()
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), NewFakeCapture(1), in, nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	if err := rt.Input(InputEvent{Op: "click", X: 0.5, Y: 0.5}); !errors.Is(err, ErrDenied) {
		t.Fatalf("view 模式应返回 ErrDenied，实际 %v", err)
	}
	if in.EventCount() != 0 {
		t.Fatal("view 模式不该有事件到达注入后端")
	}

	// 待审批的 control 会话同样拒绝
	pending := Session{ID: "s", Mode: ModeControl, Status: StatusPending}
	rt2, err := newRuntime(pending, testPolicy(), NewFakeCapture(1), in, nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt2.Close()
	if err := rt2.Input(InputEvent{Op: "click", X: 0.5, Y: 0.5}); !errors.Is(err, ErrDenied) {
		t.Fatalf("pending 会话应返回 ErrDenied，实际 %v", err)
	}
}

// control 模式下，归一化坐标要乘的是**真实屏幕尺寸**，不是编码后的画面尺寸。
func TestStreamInputUsesRealScreenSize(t *testing.T) {
	cap := NewFakeCapture(1).SetSize(1920, 1080)
	pol := testPolicy()
	pol.MaxWidth = 320 // 编码后是 320 宽，注入仍应是 1920 宽
	in := NewFakeInput()
	rt, err := newRuntime(activeSession(ModeControl), pol, cap, in, nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	waitFor(t, "抓到第一帧", time.Second, func() bool { return rt.Stats().Width == 320 })

	if err := rt.Input(InputEvent{Op: "click", X: 0.5, Y: 0.5}); err != nil {
		t.Fatalf("Input: %v", err)
	}
	screens := in.Screens()
	if len(screens) != 1 {
		t.Fatalf("应收到 1 次注入，实际 %d", len(screens))
	}
	if screens[0].Width != 1920 || screens[0].Height != 1080 {
		t.Fatalf("注入应拿到真实屏幕尺寸 1920×1080，实际 %d×%d", screens[0].Width, screens[0].Height)
	}
}

// 连续失败 5 次 → onFatal(ReasonStreamError) + 订阅通道被关闭。
func TestStreamFatalAfterFiveFailures(t *testing.T) {
	cap := NewFakeCapture(1).FailFrom(1)
	fatal := make(chan string, 1)
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), cap, NewFakeInput(), nil, func(reason string) {
		fatal <- reason
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	ch, err := rt.Frames(context.Background())
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}

	select {
	case reason := <-fatal:
		if reason != ReasonStreamError {
			t.Fatalf("onFatal 应收到 %q，实际 %q", ReasonStreamError, reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("连续失败 5 次后没有触发 onFatal")
	}

	// 通道应被关闭（读到一个零值后 ok=false）
	waitFor(t, "订阅通道关闭", 2*time.Second, func() bool {
		select {
		case _, ok := <-ch:
			return !ok
		default:
			return false
		}
	})

	if rt.Stats().Frames != 0 {
		t.Fatal("失败路径不该发出帧")
	}
	// fatal 之后再订阅应被拒
	if _, err := rt.Frames(context.Background()); err == nil {
		t.Fatal("已判死刑的运行时不该再接受订阅")
	}
}

// 单次失败只记日志，不能把流搞死。
func TestStreamSingleFailureKeepsRunning(t *testing.T) {
	cap := NewFakeCapture(4).FailAt(3)
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), cap, NewFakeInput(), nil, func(string) {
		t.Error("单次失败不该触发 onFatal")
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	time.Sleep(400 * time.Millisecond)
	if cap.GrabCount() < 6 {
		t.Fatalf("抓屏应继续，实际只抓了 %d 次", cap.GrabCount())
	}
	if rt.Stats().Frames == 0 {
		t.Fatal("失败之后应继续出帧")
	}
}

// SetQuality 立即生效：尺寸/质量字段更新，且强制出下一帧。
func TestStreamSetQuality(t *testing.T) {
	cap := NewFakeCapture(1)
	pol := testPolicy()
	pol.MaxFPS = 2 // 500ms 一帧，方便验证「不等下一个 tick」
	rt, err := newRuntime(activeSession(ModeView), pol, cap, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	waitFor(t, "首帧", 2*time.Second, func() bool { return rt.Stats().Frames >= 1 })
	before := rt.Stats()

	if err := rt.SetQuality(Quality{MaxWidth: 200, Quality: 30, FPS: 2}); err != nil {
		t.Fatalf("SetQuality: %v", err)
	}
	waitFor(t, "改画质后立刻补一帧", 400*time.Millisecond, func() bool {
		return rt.Stats().Frames > before.Frames
	})

	st := rt.Stats()
	if st.Width != 200 {
		t.Fatalf("MaxWidth 应立即生效，实际宽 %d", st.Width)
	}
	if st.Quality != 30 {
		t.Fatalf("Quality 应立即生效，实际 %d", st.Quality)
	}
}

// 订阅者拿到的帧必须能被 JPEG 解码，尺寸与 Stats 一致。
func TestStreamDeliversDecodableFrames(t *testing.T) {
	pol := testPolicy()
	pol.MaxFPS = 15
	rt, err := newRuntime(activeSession(ModeView), pol, NewFakeCapture(4), NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := rt.Frames(ctx)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	select {
	case f := <-ch:
		if len(f.JPEG) == 0 || f.Width <= 0 || f.Height <= 0 {
			t.Fatalf("帧内容不合法: %+v", f)
		}
		if _, err := thumbFromJPEG(f.JPEG); err != nil {
			t.Fatalf("帧解码失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2 秒内没有收到帧")
	}

	// ctx 取消后通道要关闭（否则前端退订会泄漏 goroutine）
	cancel()
	waitFor(t, "ctx 取消后通道关闭", 2*time.Second, func() bool {
		select {
		case _, ok := <-ch:
			return !ok
		default:
			return false
		}
	})
}

// Close 幂等，且关掉所有订阅通道。
func TestStreamCloseIdempotent(t *testing.T) {
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), NewFakeCapture(1), NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	ch, err := rt.Frames(context.Background())
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("第一次 Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("第二次 Close: %v", err)
	}
	waitFor(t, "关闭后通道关闭", time.Second, func() bool {
		select {
		case _, ok := <-ch:
			return !ok
		default:
			return false
		}
	})
	if _, err := rt.Frames(context.Background()); err == nil {
		t.Fatal("关闭后不该再接受订阅")
	}
	if err := rt.Input(InputEvent{Op: "click", X: 0.5, Y: 0.5}); !errors.Is(err, ErrDenied) {
		t.Fatalf("关闭后 Input 应返回 ErrDenied，实际 %v", err)
	}
}

func TestNewRuntimeRejectsMissingBackends(t *testing.T) {
	if _, err := newRuntime(activeSession(ModeView), testPolicy(), nil, NewFakeInput(), nil, nil); err == nil {
		t.Fatal("缺抓屏后端应报错")
	}
	if _, err := newRuntime(activeSession(ModeView), testPolicy(), NewFakeCapture(1), nil, nil, nil); err == nil {
		t.Fatal("缺注入后端应报错")
	}
}
