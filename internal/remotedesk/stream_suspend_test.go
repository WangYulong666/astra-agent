package remotedesk

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 休眠检测的帧流侧行为：拿到 errScreenSuspended 就退避 2 秒，
// 期间**不再调用**抓屏后端；醒来之后自动恢复。

// suspendCapture 是一个可控的抓屏后端：
//   - suspendCalls 里的第几次调用（1-based）返回 errScreenSuspended；
//   - 其余调用返回一帧可解码的画面（图案切换，保证不是静止帧）。
type suspendCapture struct {
	mu      sync.Mutex
	calls   int
	suspend map[int]bool
	times   []time.Time
	frames  int
	pattern int
}

func newSuspendCapture(suspendCalls ...int) *suspendCapture {
	c := &suspendCapture{suspend: map[int]bool{}, frames: 4}
	for _, n := range suspendCalls {
		c.suspend[n] = true
	}
	return c
}

func (c *suspendCapture) Info() Capability {
	return Capability{Available: true, Backend: "suspend-fake"}
}

func (c *suspendCapture) Grab(ctx context.Context, opt GrabOptions) (Frame, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.times = append(c.times, time.Now())
	c.pattern++
	pattern := c.pattern
	suspended := c.suspend[n]
	c.mu.Unlock()

	if suspended {
		return Frame{}, fmt.Errorf("%w: 假抓屏第 %d 次模拟屏幕休眠", errScreenSuspended, n)
	}
	f, _, err := encodeFrame(gradientImage(160, 90), GrabOptions{MaxWidth: opt.MaxWidth, Quality: 60})
	if err != nil {
		return Frame{}, err
	}
	_ = pattern
	return f, nil
}

func (c *suspendCapture) Close() error { return nil }

func (c *suspendCapture) stats() (calls int, times []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, append([]time.Time(nil), c.times...)
}

func TestStreamSuspendedBackoff(t *testing.T) {
	c := newSuspendCapture(1) // 第一次抓屏就「屏幕睡了」
	pol := testPolicy()
	pol.MaxFPS = 20
	rt, err := newRuntime(activeSession(ModeView), pol, c, NewFakeInput(), nil, func(string) {
		t.Error("屏幕休眠不该触发 fatal")
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	waitFor(t, "第一次抓屏", 2*time.Second, func() bool {
		n, _ := c.stats()
		return n >= 1
	})

	// 退避窗口内（1.5 秒）不该有第二次抓屏。
	time.Sleep(1500 * time.Millisecond)
	if n, _ := c.stats(); n != 1 {
		t.Fatalf("休眠退避期间不该再抓屏，实际抓了 %d 次", n)
	}

	// 退避结束后必须自动恢复。
	waitFor(t, "退避后恢复抓屏", 3*time.Second, func() bool {
		n, _ := c.stats()
		return n >= 2
	})

	_, times := c.stats()
	gap := times[1].Sub(times[0])
	if gap < streamSuspendBackoff {
		t.Fatalf("两次抓屏间隔 %s 小于退避 %s", gap, streamSuspendBackoff)
	}
	t.Logf("退避实测间隔: %s", gap.Round(time.Millisecond))

	// 恢复后继续出帧（第一次成功后 force 会推一帧）。
	waitFor(t, "恢复后出帧", 2*time.Second, func() bool { return rt.Stats().Frames >= 1 })
}

// 休眠不算「抓屏失败」：连续多次休眠错误也不能判死刑（fails 必须被清零）。
func TestStreamSuspendedDoesNotTripFatal(t *testing.T) {
	c := newSuspendCapture(1, 2, 3)
	fatal := make(chan string, 1)
	pol := testPolicy()
	pol.MaxFPS = 20
	rt, err := newRuntime(activeSession(ModeView), pol, c, NewFakeInput(), nil, func(reason string) {
		fatal <- reason
	})
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	// 三次休眠 = 三次 2 秒退避，等够 4.3 秒才能确认「还在重试、没判死刑」。
	select {
	case reason := <-fatal:
		t.Fatalf("连续休眠被判死刑（%s）", reason)
	case <-time.After(4300 * time.Millisecond):
	}
	if n, _ := c.stats(); n < 3 {
		t.Fatalf("应持续重试，实际只抓了 %d 次", n)
	}
}

// 快后端（cgo 抓屏）下默认帧率上限放宽到 20；慢后端保持 10；
// 客户端/策略显式调高的值不被压低。
func TestFastBackendWidensMaxFPS(t *testing.T) {
	pol := Policy{MaxFPS: defaultPolicyFPS, MaxWidth: 320, Quality: 60, MaxInputRate: 200}

	slow, err := newRuntime(activeSession(ModeView), pol, NewFakeCapture(1), NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer slow.Close()
	if got := slow.(*frameRuntime).pol.MaxFPS; got != defaultPolicyFPS {
		t.Fatalf("慢后端应保持 %d fps，实际 %d", defaultPolicyFPS, got)
	}

	fast, err := newRuntime(activeSession(ModeView), pol, fastFakeCapture{NewFakeCapture(1)}, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer fast.Close()
	if got := fast.(*frameRuntime).pol.MaxFPS; got != fastBackendFPS {
		t.Fatalf("快后端应放宽到 %d fps，实际 %d", fastBackendFPS, got)
	}

	// 显式要 30fps 就别压回 20。
	pol.MaxFPS = 30
	fast30, err := newRuntime(activeSession(ModeView), pol, fastFakeCapture{NewFakeCapture(1)}, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer fast30.Close()
	if got := fast30.(*frameRuntime).pol.MaxFPS; got != 30 {
		t.Fatalf("显式的 30fps 不该被改，实际 %d", got)
	}
}

// fastFakeCapture 给假抓屏打上「快后端」标记。
type fastFakeCapture struct{ *FakeCapture }

func (fastFakeCapture) fastGrab() bool { return true }

// rateCapture 模拟「快但不满帧率」的后端：每次抓屏固定花 delay。
// 它就是 cgo 后端的形状（实测 40ms/帧）。
type rateCapture struct {
	*FakeCapture
	delay time.Duration
}

func (c rateCapture) fastGrab() bool { return true }

func (c rateCapture) Grab(ctx context.Context, opt GrabOptions) (Frame, error) {
	time.Sleep(c.delay)
	return c.FakeCapture.Grab(ctx, opt)
}

func (c rateCapture) GrabThumb(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	time.Sleep(c.delay)
	return c.FakeCapture.GrabThumb(ctx, opt)
}

// 节拍必须是固定周期，而不是「抓完再睡一个间隔」。
//
// 后者在 cgo 后端下会把 20fps 压成 1/(40ms+50ms) ≈ 11fps——正好落在 15fps
// 验收线下面。这个用例就是守这条：40ms 抓屏 + 20fps 目标 → 必须跑出 ≥15fps。
func TestPumpKeepsFixedCadence(t *testing.T) {
	inner := NewFakeCapture(64)
	c := rateCapture{FakeCapture: inner, delay: 40 * time.Millisecond}
	pol := testPolicy()
	pol.MaxFPS = defaultPolicyFPS // 默认 10 → 快后端放宽到 20
	rt, err := newRuntime(activeSession(ModeView), pol, c, NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	time.Sleep(2 * time.Second)

	// 期间可能有一次首帧的额外抓取，门限留一点余量：30 次 / 2s = 15fps。
	if n := inner.GrabCount(); n < 30 {
		t.Fatalf("2 秒内只抓了 %d 次（%.1f fps），低于 15fps：节拍退化成「抓完再睡」了吗？",
			n, float64(n)/2)
	} else {
		t.Logf("2 秒抓屏 %d 次 = %.1f fps", n, float64(n)/2)
	}
}
