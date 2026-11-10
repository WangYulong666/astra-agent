package remotedesk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册解码器（gnome-screenshot 在某些环境给 gif/bmp）
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ── 抓屏骨架 ────────────────────────────────────────────────────
//
// 思路：各平台只负责「跑哪条命令、参数是什么」（capture_*.go），
// 剩下的解码 / 下采样 / JPEG 编码全部在 Go 里做，零外部依赖
// （不装 ffmpeg 也能出帧，这样 L1 监看在任何一台 macOS/Linux 上都成立）。
//
// 一个 commandCapture 会被多个会话共享，且外部截图命令都要写临时文件，
// 所以 Grab 必须串行化（mutex），否则两个会话同时 screencapture 会互相
// 覆盖/抢同一个临时文件。

const (
	// thumbW/thumbH 是帧内容对比用的灰度缩略图尺寸（48×27 ≈ 16:9）。
	thumbW, thumbH = 48, 27
	// defaultJPEGQuality 是 GrabOptions.Quality 缺省值。
	defaultJPEGQuality = 70
	// grabTimeout 是单次抓屏的兜底超时（调用方没给 ctx 上限时用）。
	grabTimeout = 10 * time.Second
	// suspendedGrabThreshold 是「屏幕睡着」的判定线：单帧超过它必然异常。
	//
	// 本机实测（Mac mini M4 / macOS 26.2）：显示器休眠时
	// CGWindowListCreateImage 单帧 940ms（正常 28–31ms），screencapture 则
	// 退出码 0 但产出全黑 PNG。两种情况都必须显式识别：不识别的话抓屏泵会
	// 以 1fps 空转把 CPU 打满，还什么画面都发不出去。
	suspendedGrabThreshold = 500 * time.Millisecond
	// suspendedReason 是「抓不到真实画面」的对外说法。两种成因（没给屏幕录制
	// 权限 / 显示器休眠）在 macOS 上症状一样，文案里都写清楚，别让用户瞎猜。
	suspendedReason = "屏幕录制权限不足（抓到全黑画面）；也可能是显示器休眠/锁屏"
)

// errScreenSuspended 表示这一帧不是真的抓到了画面：屏幕睡了（单帧 500ms+）
// 或者抓回来一片全黑（没权限 / 锁屏）。
//
// 单独一个错误类型是为了让抓屏泵能把它从「抓屏失败」里摘出来：休眠不是故障，
// 不该累加到「连续失败 5 次判死刑」上，正确反应是退避等它醒。
var errScreenSuspended = errors.New("remotedesk: 屏幕休眠/未授权，抓不到画面")

// fastGrabber 是「单帧成本远低于外部命令后端」的标记（目前只有 cgo 抓屏）。
// 帧流据此放宽默认帧率上限：screencapture 110–140ms/帧只能跑 5~7fps，
// 给它 20fps 只会让任务排队。
type fastGrabber interface {
	fastGrab() bool
}

// thumbGrabber 是可选能力：抓屏的同时顺带给出灰度缩略图，
// 让帧流不必为了做「画面有没有变」的对比去 JPEG 解码整帧。
// 没实现的 Capture（例如测试用的假后端）走 thumbFromJPEG 的通用路径。
type thumbGrabber interface {
	GrabThumb(ctx context.Context, opt GrabOptions) (Frame, []byte, error)
}

// screenSource 是可选能力：返回最近一次抓到的**原始**屏幕尺寸。
// 客户端坐标是相对编码后画面的归一化值，注入时要乘的是真实屏幕尺寸，
// 而不是缩小后的画面尺寸（MaxWidth=1600 时两者差 1.2 倍）。
type screenSource interface {
	ScreenSize() Screen
}

// defaultCapture 按 GOOS 分派到平台实现，并优先尝试「加速后端」。
//
// 加速后端（darwin+cgo 的 CGWindowListCreateImage）只有在真有符号、
// 真能抓出一帧时才被选中；取不到符号或首帧失败就**静默回退**到原来的
// 外部命令后端——回退路径是硬约束：抓屏加速失败绝不能让监看整层不可用。
//
// 非 darwin / 非 cgo 构建下 newAcceleratedCapture 恒返回 (nil, nil)，
// 这条分支编译期就没了（见 capture_accel_other.go）。
func defaultCapture(cfg Config) (Capture, error) {
	if c, err := newAcceleratedCapture(cfg); err == nil && c != nil {
		return c, nil
	}
	return newPlatformCapture(cfg)
}

// commandCapture 是「外部命令存 PNG 到临时文件 → Go 重编码」的通用后端。
type commandCapture struct {
	mu      sync.Mutex
	backend string
	binPath string
	args    func(path string) []string
	caps    capabilityCache

	lastScreen Screen
	closed     bool
}

func (c *commandCapture) Info() Capability {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps.get(c.probeLocked)
}

func (c *commandCapture) ScreenSize() Screen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastScreen
}

func (c *commandCapture) Grab(ctx context.Context, opt GrabOptions) (Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, _, err := c.grabLocked(ctx, opt)
	return f, err
}

func (c *commandCapture) GrabThumb(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grabLocked(ctx, opt)
}

func (c *commandCapture) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// probeLocked 真抓一帧来判断可用性（截图是只读的，不会改屏幕状态）。
// 注意 macOS 的坑：屏幕录制权限没给时 screencapture 往往**退出码 0**，
// 只是画面全黑。所以判可用不能只看 exit code，还要看图像亮度和方差。
func (c *commandCapture) probeLocked() Capability {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	_, thumb, err := c.grabLocked(ctx, GrabOptions{MaxWidth: 320, Quality: 50})
	if err != nil {
		if errors.Is(err, errScreenSuspended) {
			// 全黑有两种成因，实测都会碰到：没给屏幕录制权限，或者显示器在休眠
			// （本机实测：显示器休眠时 screencapture 退出码 0、画面纯黑、39KB；
			//  用 caffeinate -u 唤醒后立刻变回 2.4MB 的真实画面）。
			// 这里不做「自动唤醒」：探测每 30 秒可能被调一次，反复 caffeinate -u
			// 会让显示器永远睡不着。让用户按 Hint 自己决定。
			return Capability{
				Backend: c.backend,
				Reason:  suspendedReason,
				Hint:    hintScreenRecording + "；若已授权，先唤醒显示器再试（caffeinate -u -t 5）",
			}
		}
		return Capability{Backend: c.backend, Reason: clipReason(err.Error())}
	}
	if looksBlack(thumb) {
		return Capability{
			Backend: c.backend,
			Reason:  suspendedReason,
			Hint:    hintScreenRecording + "；若已授权，先唤醒显示器再试（caffeinate -u -t 5）",
		}
	}
	return Capability{Available: true, Backend: c.backend}
}

func (c *commandCapture) grabLocked(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	if c.closed {
		return Frame{}, nil, fmt.Errorf("%w: 抓屏后端已关闭", ErrUnavailable)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grabTimeout)
		defer cancel()
	}

	// 每帧一个新临时文件，用完立刻删——否则跑一晚上会留几十万个 PNG。
	path, err := shotTempPath()
	if err != nil {
		return Frame{}, nil, fmt.Errorf("remotedesk: 建临时文件失败: %w", err)
	}
	defer os.Remove(path)

	start := time.Now()
	cmd := exec.CommandContext(ctx, c.bin(), c.args(path)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := clipReason(stderr.String())
		if msg == "" {
			msg = clipReason(err.Error())
		}
		return Frame{}, nil, fmt.Errorf("remotedesk: %s 抓屏失败: %s", c.backend, msg)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Frame{}, nil, fmt.Errorf("remotedesk: 读截图失败: %w", err)
	}
	if len(raw) == 0 {
		return Frame{}, nil, fmt.Errorf("remotedesk: %s 产出空文件", c.backend)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Frame{}, nil, fmt.Errorf("remotedesk: 截图解码失败: %w", err)
	}

	b := src.Bounds()
	c.lastScreen = Screen{Width: b.Dx(), Height: b.Dy()}
	frame, thumb, err := encodeFrame(src, opt)
	if err != nil {
		return Frame{}, nil, err
	}
	// 全黑 = 没权限或屏幕睡了（见 suspendedReason 的注释）；单帧 500ms+ 也
	// 只可能是屏幕睡了。两种都返回 errScreenSuspended，让帧流退避而不是
	// 把它当「抓屏失败」累计到判死刑。
	if looksBlack(thumb) {
		return Frame{}, nil, fmt.Errorf("%w: %s 抓到全黑画面", errScreenSuspended, c.backend)
	}
	if elapsed := time.Since(start); elapsed >= suspendedGrabThreshold {
		return Frame{}, nil, fmt.Errorf("%w: %s 单帧耗时 %s", errScreenSuspended, c.backend, elapsed.Round(time.Millisecond))
	}
	return frame, thumb, nil
}

// bin 返回实际可执行文件路径；为空时退化成 PATH 查找。
func (c *commandCapture) bin() string {
	if c.binPath != "" {
		return c.binPath
	}
	return c.backend
}

// encodeFrame 把任意 image.Image 下采样 + 编码成 JPEG，并顺带算灰度缩略图。
func encodeFrame(src image.Image, opt GrabOptions) (Frame, []byte, error) {
	return encodeImage(downscale(src, opt.MaxWidth), opt)
}

// encodeImage 是编码段的公共实现：已经是 RGBA 的图直接编码（cgo 后端走这条，
// 省掉 downscale 里的两次整图拷贝）。
func encodeImage(img *image.RGBA, opt GrabOptions) (Frame, []byte, error) {
	q := opt.Quality
	if q <= 0 {
		q = defaultJPEGQuality
	}
	if q > 100 {
		q = 100
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return Frame{}, nil, fmt.Errorf("remotedesk: JPEG 编码失败: %w", err)
	}
	b := img.Bounds()
	return Frame{
		JPEG:   buf.Bytes(),
		Width:  b.Dx(),
		Height: b.Dy(),
		At:     time.Now(),
	}, grayThumb(img), nil
}

// downscale 用 box 平均把图缩到 maxW 宽（高按比例）。maxW<=0 或图本来就窄时只做 RGBA 化。
// 手写而不是引 golang.org/x/image：就这一个函数，不值得加依赖。
func downscale(src image.Image, maxW int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	dstW, dstH := downscaleSize(sw, sh, maxW)
	if dstW == sw && dstH == sh {
		return toOpaqueRGBA(src)
	}

	srcRGBA := toOpaqueRGBA(src)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		y0 := y * sh / dstH
		y1 := (y + 1) * sh / dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dstW; x++ {
			x0 := x * sw / dstW
			x1 := (x + 1) * sw / dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				off := sy*srcRGBA.Stride + x0*4
				for sx := x0; sx < x1; sx++ {
					r += uint32(srcRGBA.Pix[off])
					g += uint32(srcRGBA.Pix[off+1])
					bl += uint32(srcRGBA.Pix[off+2])
					off += 4
					n++
				}
			}
			i := y*dst.Stride + x*4
			if n > 0 {
				dst.Pix[i] = uint8(r / n)
				dst.Pix[i+1] = uint8(g / n)
				dst.Pix[i+2] = uint8(bl / n)
			}
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}

// toOpaqueRGBA 把任意图转成不透明 RGBA（透明区域压到黑底）。
// JPEG 没有 alpha 通道，先合成掉可以避免半透明图编码出来发灰。
func toOpaqueRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// grayThumb 把整幅画面按 box 平均压成 48×27 的 8bit 灰度图（BT.601 亮度）。
// 帧流拿它做「画面变没变」的对比：1296 字节比较，比解码 JPEG 便宜得多。
func grayThumb(src image.Image) []byte {
	img, ok := src.(*image.RGBA)
	if !ok || img.Rect.Min != (image.Point{}) {
		img = toOpaqueRGBA(src)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, thumbW*thumbH)
	if w <= 0 || h <= 0 {
		return out
	}
	for ty := 0; ty < thumbH; ty++ {
		y0 := ty * h / thumbH
		y1 := (ty + 1) * h / thumbH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for tx := 0; tx < thumbW; tx++ {
			x0 := tx * w / thumbW
			x1 := (tx + 1) * w / thumbW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum, n uint64
			for y := y0; y < y1; y++ {
				off := y*img.Stride + x0*4
				for x := x0; x < x1; x++ {
					sum += uint64(299*uint32(img.Pix[off]) + 587*uint32(img.Pix[off+1]) + 114*uint32(img.Pix[off+2]))
					off += 4
					n++
				}
			}
			if n > 0 {
				out[ty*thumbW+tx] = byte(sum / n / 1000)
			}
		}
	}
	return out
}

// meanAbsDiff 是两幅灰度缩略图的平均绝对差（0..255）。
func meanAbsDiff(a, b []byte) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 255
	}
	var sum uint64
	for i := range a {
		if a[i] > b[i] {
			sum += uint64(a[i] - b[i])
		} else {
			sum += uint64(b[i] - a[i])
		}
	}
	return float64(sum) / float64(len(a))
}

// looksBlack 判断是不是「纯黑帧」——macOS 没给屏幕录制权限时的典型症状。
// 只认全 0（mean 和方差都为 0）：全白的桌面是合法画面，不能一起判死。
func looksBlack(thumb []byte) bool {
	if len(thumb) == 0 {
		return true
	}
	for _, v := range thumb {
		if v != 0 {
			return false
		}
	}
	return true
}

// thumbFromJPEG 是给「没实现 thumbGrabber 的 Capture」用的通用路径。
func thumbFromJPEG(data []byte) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("remotedesk: 帧解码失败: %w", err)
	}
	return grayThumb(img), nil
}

func shotTempPath() (string, error) {
	f, err := os.CreateTemp("", "astra-rd-*.png")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// unavailableCapture 是「本机/本平台没有抓屏能力」的占位实现。
// 它不返回 error，而是让 Info() 老实说不可用——能力探测接口要能正常回话。
type unavailableCapture struct{ cap Capability }

func (u unavailableCapture) Info() Capability { return u.cap }

func (u unavailableCapture) Grab(context.Context, GrabOptions) (Frame, error) {
	return Frame{}, fmt.Errorf("%w: %s", ErrUnavailable, u.cap.Reason)
}

func (u unavailableCapture) Close() error { return nil }
