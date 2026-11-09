package remotedesk

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func gradientImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / w),
				G: uint8(y * 255 / h),
				B: 128,
				A: 0xff,
			})
		}
	}
	return img
}

func TestEncodeFrameResizesAndDecodes(t *testing.T) {
	src := gradientImage(800, 600)
	f, thumb, err := encodeFrame(src, GrabOptions{MaxWidth: 400, Quality: 60})
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	if f.Width != 400 || f.Height != 300 {
		t.Fatalf("尺寸应等比缩到 400×300，实际 %d×%d", f.Width, f.Height)
	}
	if len(thumb) != thumbW*thumbH {
		t.Fatalf("缩略图应为 %d 字节，实际 %d", thumbW*thumbH, len(thumb))
	}
	if f.At.IsZero() {
		t.Fatal("Frame.At 未填")
	}

	img, err := jpeg.Decode(bytes.NewReader(f.JPEG))
	if err != nil {
		t.Fatalf("产出的帧不是合法 JPEG: %v", err)
	}
	if got := img.Bounds(); got.Dx() != 400 || got.Dy() != 300 {
		t.Fatalf("JPEG 实际尺寸 %d×%d", got.Dx(), got.Dy())
	}
}

func TestEncodeFrameKeepsSmallImage(t *testing.T) {
	f, _, err := encodeFrame(gradientImage(120, 90), GrabOptions{MaxWidth: 1600, Quality: 60})
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	if f.Width != 120 || f.Height != 90 {
		t.Fatalf("图比 MaxWidth 小的时候不该放大，实际 %d×%d", f.Width, f.Height)
	}
}

func TestEncodeFrameQualityAffectsSize(t *testing.T) {
	src := gradientImage(640, 480)
	low, _, err := encodeFrame(src, GrabOptions{MaxWidth: 640, Quality: 20})
	if err != nil {
		t.Fatalf("低质量编码失败: %v", err)
	}
	high, _, err := encodeFrame(src, GrabOptions{MaxWidth: 640, Quality: 92})
	if err != nil {
		t.Fatalf("高质量编码失败: %v", err)
	}
	if len(high.JPEG) <= len(low.JPEG) {
		t.Fatalf("高质量应产生更大字节数：q92=%d q20=%d", len(high.JPEG), len(low.JPEG))
	}
}

func TestDownscaleBoxAverage(t *testing.T) {
	// 左半边白、右半边黑，缩一半后每个目标像素正好取 2×2 的平均。
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			c := color.RGBA{0, 0, 0, 0xff}
			if x < 2 {
				c = color.RGBA{255, 255, 255, 0xff}
			}
			src.Set(x, y, c)
		}
	}
	dst := downscale(src, 2)
	if got := dst.Bounds(); got.Dx() != 2 || got.Dy() != 2 {
		t.Fatalf("下采样尺寸 %v", got)
	}
	if v := dst.Pix[0]; v < 250 {
		t.Fatalf("左上应接近白色，实际 %d", v)
	}
	if v := dst.Pix[4]; v > 5 {
		t.Fatalf("右上应接近黑色，实际 %d", v)
	}
}

func TestGrayThumbAndDiff(t *testing.T) {
	white := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for i := range white.Pix {
		white.Pix[i] = 0xff
	}
	black := image.NewRGBA(image.Rect(0, 0, 160, 90))

	tw := grayThumb(white)
	tb := grayThumb(black)

	if looksBlack(tw) {
		t.Fatal("全白画面不该被判成全黑")
	}
	if !looksBlack(tb) {
		t.Fatal("全黑画面应被判成全黑")
	}
	if d := meanAbsDiff(tw, tw); d != 0 {
		t.Fatalf("同一幅图的差分应为 0，实际 %f", d)
	}
	if d := meanAbsDiff(tw, tb); d < 200 {
		t.Fatalf("黑白两幅图的差分应该很大，实际 %f", d)
	}
	if d := meanAbsDiff(tw, nil); d != 255 {
		t.Fatalf("长度不一致时差分应为 255，实际 %f", d)
	}
}

func TestUnavailableCapture(t *testing.T) {
	cap := unavailableCapture{Capability{Reason: "当前平台不支持抓屏"}}
	if cap.Info().Available {
		t.Fatal("不该报可用")
	}
	if _, err := cap.Grab(context.Background(), GrabOptions{}); err == nil {
		t.Fatal("Grab 应返回错误")
	} else if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("应包装 ErrUnavailable，实际 %v", err)
	}
	if err := cap.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// 真机用例：本任务唯一能证明「画面真能出来」的测试。
// 只在 macOS 且有 screencapture 时跑，其它环境跳过并说明原因。
//
// 注意：显示器休眠时 screencapture 照样退出码 0，但抓到的是纯黑画面
// （本机实测：休眠时 39KB 全黑，唤醒后 2.4MB 真实画面）。所以这里先
// 用 caffeinate -u 唤醒显示器 —— 只持续 2 秒，之后按系统设置重新休眠。
func TestRealScreencapture(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("非 darwin 平台，真机抓屏用例不适用")
	}
	if _, err := exec.LookPath("screencapture"); err != nil {
		t.Skip("未找到 screencapture，跳过真机抓屏")
	}
	wakeDisplay(t)

	cap, err := defaultCapture(Config{})
	if err != nil {
		t.Fatalf("defaultCapture: %v", err)
	}
	defer cap.Close()

	info := cap.Info()
	t.Logf("Info: available=%v backend=%q reason=%q hint=%q",
		info.Available, info.Backend, info.Reason, info.Hint)
	if !info.Available {
		if strings.Contains(info.Reason, "全黑") {
			t.Skipf("显示器处于休眠/锁屏且未能唤醒，本机无法产出真实画面：%s", info.Reason)
		}
		t.Fatalf("真机抓屏应可用，实际不可用：reason=%q hint=%q", info.Reason, info.Hint)
	}
	if info.Backend != "screencapture" {
		t.Fatalf("Backend 应为 screencapture，实际 %q", info.Backend)
	}

	start := time.Now()
	f, err := cap.Grab(context.Background(), GrabOptions{MaxWidth: 1280, Quality: 60})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("真机 Grab 失败: %v", err)
	}
	if f.Width <= 0 || f.Height <= 0 {
		t.Fatalf("帧尺寸非法: %d×%d", f.Width, f.Height)
	}

	img, err := jpeg.Decode(bytes.NewReader(f.JPEG))
	if err != nil {
		t.Fatalf("真机帧不是合法 JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != f.Width || b.Dy() != f.Height {
		t.Fatalf("Frame 声明的尺寸 %d×%d 与 JPEG 实际 %d×%d 不一致",
			f.Width, f.Height, b.Dx(), b.Dy())
	}
	if looksBlack(grayThumb(img)) {
		t.Fatal("抓到的画面全黑（屏幕录制权限不足？）")
	}

	screen := Screen{Width: f.Width, Height: f.Height}
	if ss, ok := cap.(screenSource); ok {
		screen = ss.ScreenSize()
	}
	t.Logf("真机 Grab 结果：编码尺寸=%d×%d，原始屏幕=%d×%d，JPEG=%d 字节，耗时=%s",
		f.Width, f.Height, screen.Width, screen.Height, len(f.JPEG), elapsed.Round(time.Millisecond))

	// 连抓第二帧：验证串行化 + 临时文件每帧清理（没有互相覆盖/残留报错）。
	start = time.Now()
	f2, err := cap.Grab(context.Background(), GrabOptions{MaxWidth: 640, Quality: 45})
	if err != nil {
		t.Fatalf("连续第二次 Grab 失败: %v", err)
	}
	t.Logf("第二帧（640 宽 q45）：%d×%d，%d 字节，耗时=%s",
		f2.Width, f2.Height, len(f2.JPEG), time.Since(start).Round(time.Millisecond))
	if f2.Width != 640 {
		t.Fatalf("MaxWidth=640 生效失败，实际宽 %d", f2.Width)
	}
}

// wakeDisplay 用 caffeinate -u 模拟一次用户活动把显示器叫醒（只持续 2 秒）。// 只在真机用例里用：探测流程本身绝不唤醒屏幕，否则前端轮询 capabilities
// 会导致显示器永远睡不着。
func wakeDisplay(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("caffeinate"); err != nil {
		t.Log("未找到 caffeinate，跳过唤醒显示器")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "caffeinate", "-u", "-t", "2").Run()
	time.Sleep(300 * time.Millisecond)
}

// BenchmarkEncodeFrame 量的是抓屏链路里我们自己那段（下采样 + JPEG 编码）的成本，
// 用来判断 screencapture 之外还剩多少预算。
func BenchmarkEncodeFrame(b *testing.B) {
	src := gradientImage(1920, 1080)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := encodeFrame(src, GrabOptions{MaxWidth: 1280, Quality: 60}); err != nil {
			b.Fatal(err)
		}
	}
}
