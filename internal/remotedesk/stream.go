package remotedesk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ── 帧流 ────────────────────────────────────────────────────────
//
// 一个 Runtime = 一个手机客户端的通道。抓屏泵是这一路唯一的抓屏者，
// 多个订阅者共享它（同一会话开两个页面看同一块屏幕，不会抓两次）。
//
// 三条硬约束：
//  1. 抓屏泵**永不阻塞在慢客户端上**：订阅通道缓冲 1，满了丢最旧的帧；
//  2. 静止画面不发帧（48×27 灰度差分），但每 2 秒强发一帧心跳，
//     否则手机端会判定「流断了」；
//  3. 连续失败 5 次才判死刑（onFatal），单次抖动只记日志——
//     屏幕休眠/锁屏/切用户的瞬间抓屏失败是常态。

const (
	// streamHeartbeat 是静止画面的强发间隔。
	streamHeartbeat = 2 * time.Second
	// streamDiffThresh 是「画面变了」的平均绝对差阈值（0..255）。
	streamDiffThresh = 3.0
	// streamFatalLimit 是连续失败几次判死刑。
	streamFatalLimit = 5
	// streamGrabTimeout 是单次抓屏超时（screencapture 正常 100~400ms）。
	streamGrabTimeout = 8 * time.Second
	// streamCloseWait 是 Close 等泵退出的上限，防止异常后端把 Close 挂死。
	streamCloseWait = 12 * time.Second
	// streamSuspendBackoff 是「屏幕睡了」时的退避间隔。
	//
	// 休眠时单帧 940ms（cgo）或出全黑图（screencapture），按正常帧率空转等于
	// 把 CPU 打满还发不出画面。2 秒一次足够在屏幕醒来后立刻恢复。
	streamSuspendBackoff = 2 * time.Second
	// defaultPolicyFPS 是 Policy.WithDefaults 的 MaxFPS 默认值（10）。
	// 它按 screencapture 的 110–140ms/帧定的；换成 cgo 后端后默认值太保守。
	defaultPolicyFPS = 10
	// fastBackendFPS 是 cgo 后端下的默认上限。
	//
	// 实测 44–47ms/帧（抓屏 28–31 + 下采样与编码 16）→ 上限 21–23fps，
	// 取 20 留一点余量。它**不是**突破 Policy：Policy 显式调高更高值仍然生效，
	// 只是 ≤10 的值（几乎都是「没配」的默认值）在快后端上被放宽。
	fastBackendFPS = 20

	inputRateWindow   = time.Second
	inputRateLogEvery = time.Minute
)

// newRuntime 构造一路帧流（签名由 R1 的主控契约固定）。
func newRuntime(sess Session, pol Policy, cap Capture, in Input, lg *log.Logger, onFatal func(reason string)) (Runtime, error) {
	if cap == nil {
		return nil, fmt.Errorf("remotedesk: 缺少抓屏后端")
	}
	if in == nil {
		return nil, fmt.Errorf("remotedesk: 缺少注入后端")
	}
	pol = pol.WithDefaults()
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	// 快后端（cgo 抓屏）下把默认 10fps 放宽到 20：5fps 的手感问题一半出在这里。
	if _, ok := cap.(fastGrabber); ok && pol.MaxFPS <= defaultPolicyFPS {
		pol.MaxFPS = fastBackendFPS
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &frameRuntime{
		sess:     sess,
		pol:      pol,
		cap:      cap,
		in:       in,
		lg:       lg,
		onFatal:  onFatal,
		ctx:      ctx,
		cancel:   cancel,
		subs:     make(map[int]*subscriber),
		wake:     make(chan struct{}, 1),
		pumpDone: make(chan struct{}),
		limiter:  inputLimiter{max: pol.MaxInputRate, window: inputRateWindow},
		opt:      GrabOptions{MaxWidth: pol.MaxWidth, Quality: pol.Quality},
	}
	r.quality.Store(int64(pol.Quality))
	r.intervalNanos.Store(int64(time.Second) / int64(pol.MaxFPS))
	go r.pump()
	return r, nil
}

type subscriber struct {
	ch chan Frame
}

type frameRuntime struct {
	sess    Session
	pol     Policy
	cap     Capture
	in      Input
	lg      *log.Logger
	onFatal func(reason string)

	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	pumpDone chan struct{}

	mu        sync.Mutex
	subs      map[int]*subscriber
	nextID    int
	opt       GrabOptions
	force     bool
	prevThumb []byte
	screen    Screen

	closed    atomic.Bool
	closeOnce sync.Once
	fatalOnce sync.Once

	intervalNanos atomic.Int64
	quality       atomic.Int64
	width         atomic.Int64
	height        atomic.Int64
	framesSent    atomic.Uint64
	dropped       atomic.Uint64
	inputs        atomic.Uint64

	// rtc 是这一路的 WebRTC 通道（懒创建：客户端发 offer 才有）。
	// 它只影响「额外怎么送帧」，不影响 WS 的 binary 帧——两条路并行。
	rtcMu sync.Mutex
	rtc   *rtcSession

	fps     fpsMeter
	limiter inputLimiter
}

// ── Runtime 接口 ────────────────────────────────────────────────

func (r *frameRuntime) Frames(ctx context.Context) (<-chan Frame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.closed.Load() {
		return nil, fmt.Errorf("%w: 帧流已关闭", ErrDenied)
	}

	sub := &subscriber{ch: make(chan Frame, 1)}
	r.mu.Lock()
	if r.closed.Load() {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: 帧流已关闭", ErrDenied)
	}
	r.nextID++
	id := r.nextID
	r.subs[id] = sub
	// 新订阅者立刻要一帧，否则静止画面下第一次出图要等 2 秒心跳。
	r.force = true
	r.mu.Unlock()
	r.poke()

	go func() {
		select {
		case <-ctx.Done():
			r.unsubscribe(id)
		case <-r.ctx.Done():
			// 运行时已关闭，通道由 closeSubs 关掉了
		}
	}()

	return sub.ch, nil
}

func (r *frameRuntime) Input(ev InputEvent) error {
	if r.closed.Load() {
		return fmt.Errorf("%w: 帧流已关闭", ErrDenied)
	}
	if r.pol.DisableInput {
		return fmt.Errorf("%w: 本机策略禁止键鼠注入", ErrDenied)
	}
	if r.sess.Status != StatusActive || r.sess.Mode != ModeControl {
		return fmt.Errorf("%w: 会话不可控（status=%s mode=%s）", ErrDenied, r.sess.Status, r.sess.Mode)
	}
	if err := sanitizeEvent(&ev, r.lg); err != nil {
		return err
	}

	now := time.Now()
	if !r.limiter.allow(now) {
		// 超速属于「客户端有毛病」，一分钟只记一次日志，别把日志刷爆。
		if r.limiter.shouldLog(now) {
			r.logf("输入超速，已丢弃（上限 %d/s）", r.pol.MaxInputRate)
		}
		return fmt.Errorf("%w: 输入超速（上限 %d/s）", ErrDenied, r.pol.MaxInputRate)
	}

	ctx, cancel := context.WithTimeout(r.ctx, inputApplyTimeout)
	defer cancel()
	if err := r.in.Apply(ctx, ev, r.screenSize()); err != nil {
		return err
	}
	r.inputs.Add(1)
	return nil
}

func (r *frameRuntime) SetQuality(q Quality) error {
	if r.closed.Load() {
		return fmt.Errorf("%w: 帧流已关闭", ErrDenied)
	}
	r.mu.Lock()
	if q.MaxWidth > 0 {
		r.opt.MaxWidth = clampInt(q.MaxWidth, 160, 3840)
	}
	if q.Quality > 0 {
		r.opt.Quality = clampInt(q.Quality, 1, 100)
		r.quality.Store(int64(r.opt.Quality))
	}
	if q.FPS > 0 {
		// 服务端策略是上限，客户端只能往下调（10 路手机同时看时别把机器抓死）。
		fps := clampInt(q.FPS, 1, r.pol.MaxFPS)
		r.intervalNanos.Store(int64(time.Second) / int64(fps))
	}
	// 参数变了就必须立刻出一帧，否则用户改完画质看不到变化。
	r.force = true
	r.prevThumb = nil
	r.mu.Unlock()
	r.poke()
	return nil
}

func (r *frameRuntime) Stats() Stats {
	return Stats{
		FPS:     r.fps.value(time.Now()),
		Width:   int(r.width.Load()),
		Height:  int(r.height.Load()),
		Quality: int(r.quality.Load()),
		Frames:  r.framesSent.Load(),
		Dropped: r.dropped.Load(),
		Inputs:  r.inputs.Load(),
	}
}

func (r *frameRuntime) Close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.cancel()
		// RTC 通道（如果客户端开了）要一起收掉：PC 不关会一直占着 UDP 端口
		// 和两条后台 goroutine。
		r.closeRTC()
		r.closeSubs()
		// 等泵退出，保证不留 goroutine（泵里的抓屏会因 ctx 取消而中断）。
		select {
		case <-r.pumpDone:
		case <-time.After(streamCloseWait):
			r.logf("抓屏泵未在 %s 内退出（后端忽略了 ctx）", streamCloseWait)
		}
	})
	return nil
}

// ── 抓屏泵 ──────────────────────────────────────────────────────

// pump 是唯一抓屏者。
//
// 节拍是**固定周期**而不是「抓完再睡一个间隔」：后者每帧的实际周期是
// 抓屏耗时 + 间隔。screencapture 时代（110–140ms）这个错误被掩盖了，
// cgo 后端（40ms）下它会把 20fps 压成 11fps——正好卡在 15fps 验收线以下。
func (r *frameRuntime) pump() {
	defer close(r.pumpDone)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	fails := 0
	var lastSent time.Time
	// next 是下一帧的**目标时刻**（不是「还要等多久」）。
	var next time.Time
	// suspended 是「屏幕睡眠」的状态，只用来保证日志打一次：
	// 醒来第一帧会打一行「恢复」，之后不再重复。
	suspended := false

	for {
		// 等到目标时刻；wake 可以提前叫醒（改画质/新订阅要立刻出一帧）。
		if wait := time.Until(next); wait > 0 {
			timer.Reset(wait)
			select {
			case <-r.ctx.Done():
				return
			case <-timer.C:
			case <-r.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
		} else {
			select {
			case <-r.ctx.Done():
				return
			case <-r.wake:
			default:
			}
		}

		start := time.Now()
		opt, force, prev := r.takeGrabState()
		frame, thumb, err := r.grab(opt)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			// 屏幕睡了不是「抓屏故障」：不累加 fails（否则 5 次就判死刑，
			// 而屏幕休眠是正常的省电行为），退避等它醒。
			if errors.Is(err, errScreenSuspended) {
				if !suspended {
					suspended = true
					r.logf("屏幕休眠/未授权，暂停抓屏并每 %s 重试一次（恢复后自动继续）：%v",
						streamSuspendBackoff, err)
				}
				fails = 0
				next = time.Now().Add(streamSuspendBackoff)
				continue
			}
			fails++
			r.logf("抓屏失败（连续第 %d 次）：%v", fails, err)
			if fails >= streamFatalLimit {
				r.fatal(ReasonStreamError)
				return
			}
			next = time.Now().Add(r.frameInterval())
			continue
		}
		if suspended {
			suspended = false
			r.logf("屏幕已恢复，继续抓屏")
		}
		fails = 0

		now := time.Now()
		changed := prev == nil || len(prev) != len(thumb) || meanAbsDiff(prev, thumb) >= streamDiffThresh
		heartbeat := lastSent.IsZero() || now.Sub(lastSent) >= streamHeartbeat
		if force || changed || heartbeat {
			r.broadcast(frame)
			lastSent = now
		}
		r.setPrevThumb(thumb)

		// 目标时刻按「这一帧开始的时间」推一格：抓得比间隔快就睡剩下的部分，
		// 抓得慢就立刻再抓（绝不补攒，否则延迟越积越大）。
		next = start.Add(r.frameInterval())
		if next.Before(time.Now()) {
			next = time.Now()
		}
	}
}

func (r *frameRuntime) takeGrabState() (GrabOptions, bool, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	force := r.force
	r.force = false
	return r.opt, force, r.prevThumb
}

func (r *frameRuntime) setPrevThumb(t []byte) {
	r.mu.Lock()
	r.prevThumb = t
	r.mu.Unlock()
}

func (r *frameRuntime) frameInterval() time.Duration {
	n := r.intervalNanos.Load()
	if n <= 0 {
		return time.Second / time.Duration(r.pol.MaxFPS)
	}
	return time.Duration(n)
}

// poke 让泵立刻醒来一次（非阻塞：泵正忙时丢掉这次唤醒没关系，
// 它总会在下一个 tick 看到 force 标记）。
func (r *frameRuntime) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *frameRuntime) grab(opt GrabOptions) (Frame, []byte, error) {
	ctx, cancel := context.WithTimeout(r.ctx, streamGrabTimeout)
	defer cancel()

	var (
		frame Frame
		thumb []byte
		err   error
	)
	if tg, ok := r.cap.(thumbGrabber); ok {
		// 内置后端顺手给出缩略图，省一次全帧 JPEG 解码。
		frame, thumb, err = tg.GrabThumb(ctx, opt)
	} else {
		frame, err = r.cap.Grab(ctx, opt)
		if err == nil {
			thumb, err = thumbFromJPEG(frame.JPEG)
		}
	}
	if err != nil {
		return Frame{}, nil, err
	}
	r.noteFrame(frame)
	return frame, thumb, nil
}

// noteFrame 记录编码尺寸与真实屏幕尺寸（注入换算要用后者）。
func (r *frameRuntime) noteFrame(f Frame) {
	if f.Width > 0 {
		r.width.Store(int64(f.Width))
	}
	if f.Height > 0 {
		r.height.Store(int64(f.Height))
	}
	r.mu.Lock()
	if ss, ok := r.cap.(screenSource); ok {
		if s := ss.ScreenSize(); s.Width > 0 && s.Height > 0 {
			r.screen = s
		}
	}
	if r.screen.Width <= 0 && f.Width > 0 {
		// 拿不到真实屏幕尺寸时退化成编码尺寸（假后端/测试场景）。
		r.screen = Screen{Width: f.Width, Height: f.Height}
	}
	r.mu.Unlock()
}

func (r *frameRuntime) screenSize() Screen {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.screen
	if s.Width <= 0 || s.Height <= 0 {
		s = Screen{Width: int(r.width.Load()), Height: int(r.height.Load())}
	}
	return s
}

// broadcast 把帧塞给所有订阅者。缓冲只有 1：满了就丢旧帧再放新的，
// 于是慢客户端只会看到「跳帧」，抓屏泵一点都不受影响。
func (r *frameRuntime) broadcast(f Frame) {
	r.mu.Lock()
	for _, s := range r.subs {
		select {
		case s.ch <- f:
			continue
		default:
		}
		select {
		case <-s.ch: // 丢掉最旧的一帧
		default:
		}
		select {
		case s.ch <- f:
		default:
		}
		r.dropped.Add(1)
	}
	r.mu.Unlock()

	r.framesSent.Add(1)
	r.fps.add(time.Now())
}

func (r *frameRuntime) unsubscribe(id int) {
	r.mu.Lock()
	if s, ok := r.subs[id]; ok {
		delete(r.subs, id)
		close(s.ch)
	}
	r.mu.Unlock()
}

func (r *frameRuntime) closeSubs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.subs {
		delete(r.subs, id)
		close(s.ch)
	}
}

// fatal 是「这一路彻底废了」：通知上层（R1 结束会话），关掉所有订阅通道。
func (r *frameRuntime) fatal(reason string) {
	r.fatalOnce.Do(func() {
		r.logf("抓屏连续失败 %d 次，关闭帧流：%s", streamFatalLimit, reason)
		r.closed.Store(true)
		r.cancel()
		r.closeRTC()
		r.closeSubs()
		if r.onFatal != nil {
			r.onFatal(reason)
		}
	})
}

func (r *frameRuntime) logf(format string, args ...any) {
	if r.lg != nil {
		r.lg.Printf(format, args...)
	}
}

// ── 统计 ────────────────────────────────────────────────────────

// fpsMeter 算「最近 1 秒实际发出多少帧」。
type fpsMeter struct {
	mu    sync.Mutex
	start time.Time
	n     int
	last  float64
}

func (m *fpsMeter) add(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start.IsZero() {
		m.start = now
		m.n = 1
		return
	}
	if el := now.Sub(m.start); el >= time.Second {
		m.last = float64(m.n) / el.Seconds()
		m.start = now
		m.n = 1
		return
	}
	m.n++
}

func (m *fpsMeter) value(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.start.IsZero() {
		return 0
	}
	// 一段时间没发帧，FPS 必须掉到 0，不能一直显示旧的峰值。
	if el := now.Sub(m.start); el >= time.Second {
		m.last = float64(m.n) / el.Seconds()
		m.start = now
		m.n = 0
	}
	return m.last
}

// inputLimiter 是 1 秒滑动窗口限速。
type inputLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	times   []time.Time
	lastLog time.Time
}

func (l *inputLimiter) allow(now time.Time) bool {
	if l.max <= 0 {
		return true
	}
	if l.window <= 0 {
		l.window = inputRateWindow
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	cut := now.Add(-l.window)
	i := 0
	for i < len(l.times) && l.times[i].Before(cut) {
		i++
	}
	if i > 0 {
		l.times = append(l.times[:0], l.times[i:]...)
	}
	if len(l.times) >= l.max {
		return false
	}
	l.times = append(l.times, now)
	return true
}

// shouldLog 保证「超速日志一分钟只记一次」。
func (l *inputLimiter) shouldLog(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.lastLog.IsZero() && now.Sub(l.lastLog) < inputRateLogEvery {
		return false
	}
	l.lastLog = now
	return true
}
