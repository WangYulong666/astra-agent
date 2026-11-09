package remotedesk

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"sync"
)

// 假后端：测试用，也被 R1（会话/审批）和 R3（API/WS）复用。
//
// 设计要点：
//   - FakeCapture 的画面是**可预测**的：第 i 帧的底色、色条宽度、编号块位置
//     都由 i 决定，同一 i 永远渲染出同样的图 —— 于是「帧内容对比」能不能
//     正确跳过静止画面，可以在无屏幕权限的机器上稳定测出来。
//   - 支持「第 N 次 Grab 报错」，用来测连续失败 5 次触发 onFatal。

// FakeCapture 是一个不需要任何系统权限的抓屏后端。
type FakeCapture struct {
	mu sync.Mutex

	frames int // 图案周期：第 i 帧的图案是 i % frames
	grabs  int // 已调用次数
	width  int
	height int
	black  bool

	failAt   map[int]bool // 指定某几次 Grab 失败（1-based）
	failFrom int          // 从第 N 次起全部失败（1-based），0 表示不启用

	closed bool
}

// NewFakeCapture 造一个会产生 frames 种图案的假抓屏（frames<1 时按 1 处理）。
// 每次 Grab 返回下一帧，循环使用这些图案。
func NewFakeCapture(frames int) *FakeCapture {
	if frames < 1 {
		frames = 1
	}
	return &FakeCapture{frames: frames, width: 320, height: 180}
}

// NewBlackCapture 模拟「macOS 没给屏幕录制权限」：能抓到帧，但全是黑的。
// Info() 因此报不可用（Reason 与真机探测一致），Grab 仍然返回全黑帧，
// 这样上层的错误路径和帧流路径都能被测到。
func NewBlackCapture() *FakeCapture {
	c := NewFakeCapture(1)
	c.black = true
	return c
}

// SetSize 调整假屏幕的原始尺寸（默认 320×180）。下载样式的注入换算测试用得上。
func (f *FakeCapture) SetSize(w, h int) *FakeCapture {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w > 0 && h > 0 {
		f.width, f.height = w, h
	}
	return f
}

// FailAt 让第 n 次（1-based）Grab 返回错误。
func (f *FakeCapture) FailAt(n int) *FakeCapture {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > 0 {
		if f.failAt == nil {
			f.failAt = map[int]bool{}
		}
		f.failAt[n] = true
	}
	return f
}

// FailFrom 让第 n 次（1-based）起的每次 Grab 都失败，用于测连续失败判死刑。
func (f *FakeCapture) FailFrom(n int) *FakeCapture {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFrom = n
	return f
}

// GrabCount 返回已调用次数。
func (f *FakeCapture) GrabCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grabs
}

func (f *FakeCapture) Info() Capability {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.black {
		return Capability{
			Backend: "fake-black",
			Reason:  "屏幕录制权限不足（抓到全黑画面）；也可能是显示器休眠/锁屏",
			Hint:    hintScreenRecording,
		}
	}
	return Capability{Available: true, Backend: "fake"}
}

func (f *FakeCapture) ScreenSize() Screen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Screen{Width: f.width, Height: f.height}
}

func (f *FakeCapture) Grab(ctx context.Context, opt GrabOptions) (Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	frame, _, err := f.grabLocked(ctx, opt)
	return frame, err
}

func (f *FakeCapture) GrabThumb(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grabLocked(ctx, opt)
}

func (f *FakeCapture) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *FakeCapture) grabLocked(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	if f.closed {
		return Frame{}, nil, fmt.Errorf("%w: 假抓屏已关闭", ErrUnavailable)
	}
	f.grabs++
	n := f.grabs
	if f.failAt[n] || (f.failFrom > 0 && n >= f.failFrom) {
		return Frame{}, nil, fmt.Errorf("remotedesk: 假抓屏第 %d 次故意失败", n)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Frame{}, nil, err
		}
	}
	frame, thumb, err := encodeFrame(f.render((n-1)%f.frames), opt)
	if err != nil {
		return Frame{}, nil, err
	}
	return frame, thumb, nil
}

// render 画一帧可预测的图案：底色 + 白色进度条 + 编号块，都由 i 决定。
func (f *FakeCapture) render(i int) image.Image {
	w, h := f.width, f.height
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if f.black {
		return img // 全 0：RGBA 零值就是全黑
	}

	bg := color.RGBA{
		R: uint8(30 + (i*37)%180),
		G: uint8(60 + (i*53)%160),
		B: uint8(90 + (i*71)%140),
		A: 0xff,
	}
	for y := 0; y < h; y++ {
		off := y * img.Stride
		for x := 0; x < w; x++ {
			px := img.Pix[off : off+4]
			px[0], px[1], px[2], px[3] = bg.R, bg.G, bg.B, 0xff
			off += 4
		}
	}

	// 进度条：宽度随帧号增长，保证不同帧的缩略图差异远超阈值
	barW := (i + 1) * w / (f.frames + 1)
	fillRect(img, 0, h/3, barW, h/2, color.RGBA{0xff, 0xff, 0xff, 0xff})

	// 编号块：位置随帧号跑动
	block := h / 6
	bx := (i * 17) % max(1, w-block)
	fillRect(img, bx, block, block*2, block, color.RGBA{0, 0, 0, 0xff})
	return img
}

func fillRect(img *image.RGBA, x0, y0, w, h int, c color.RGBA) {
	b := img.Bounds()
	for y := y0; y < y0+h; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := x0; x < x0+w; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			off := y*img.Stride + x*4
			img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3] = c.R, c.G, c.B, 0xff
		}
	}
}

// ── FakeInput ───────────────────────────────────────────────────

// FakeInput 记录所有收到的 InputEvent，供断言（速率、白名单、坐标换算…）。
type FakeInput struct {
	mu        sync.Mutex
	events    []InputEvent
	screens   []Screen
	available bool
	reason    string
	applyErr  error
	closed    bool
}

// NewFakeInput 造一个可用的假注入后端。
func NewFakeInput() *FakeInput { return &FakeInput{available: true} }

func (f *FakeInput) Info() Capability {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.available {
		reason := f.reason
		if reason == "" {
			reason = "permission_denied"
		}
		return Capability{Backend: "fake", Reason: reason, Hint: hintAccessibility}
	}
	return Capability{Available: true, Backend: "fake"}
}

func (f *FakeInput) Apply(ctx context.Context, ev InputEvent, screen Screen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("%w: 假注入已关闭", ErrUnavailable)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if f.applyErr != nil {
		return f.applyErr
	}
	f.events = append(f.events, ev)
	f.screens = append(f.screens, screen)
	return nil
}

func (f *FakeInput) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// Events 返回收到的事件副本。
func (f *FakeInput) Events() []InputEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InputEvent(nil), f.events...)
}

// EventCount 返回收到的事件数。
func (f *FakeInput) EventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// Screens 返回每次 Apply 传入的屏幕尺寸副本。
func (f *FakeInput) Screens() []Screen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Screen(nil), f.screens...)
}

// SetApplyError 让后续 Apply 一律返回该错误（测 ErrUnavailable 的透传）。
func (f *FakeInput) SetApplyError(err error) {
	f.mu.Lock()
	f.applyErr = err
	f.mu.Unlock()
}

// SetAvailable 改可用性（模拟「辅助功能权限被撤销」）。
func (f *FakeInput) SetAvailable(ok bool, reason string) {
	f.mu.Lock()
	f.available, f.reason = ok, reason
	f.mu.Unlock()
}

// NewUnavailableInput 返回一个恒不可用的注入后端（缺权限环境的演示/测试）。
func NewUnavailableInput() Input {
	return unavailableInput{Capability{
		Backend: "none",
		Reason:  "permission_denied",
		Hint:    hintAccessibility,
	}}
}
