//go:build darwin && cgo

package remotedesk

import (
	"bytes"
	"context"
	"image/jpeg"
	"testing"
	"time"
)

// TestRealCGOBackend 是真机用例：这一轮「5fps → 15fps+」的全部依据都在这里。
//
// 断言两件事：
//  1. defaultCapture 在 darwin+cgo 下真的选到了 cgo 后端（Backend=="cgo-cgwindow"）；
//  2. 连抓 20 次平均单帧 < 80ms（实测 35–47ms；门槛给 80ms 是留了机器负载余量）。
//
// 屏幕休眠时该 API 单帧 940ms，所以先用 caffeinate -u 唤醒 3 秒再测。
func TestRealCGOBackend(t *testing.T) {
	wakeDisplay(t)

	cap, err := defaultCapture(Config{})
	if err != nil {
		t.Fatalf("defaultCapture: %v", err)
	}
	defer cap.Close()

	info := cap.Info()
	if info.Backend != "cgo-cgwindow" {
		t.Fatalf("darwin+cgo 下应优先选 cgo 抓屏，实际后端 %q（reason=%q hint=%q）",
			info.Backend, info.Reason, info.Hint)
	}
	if !info.Available {
		if bytes.Contains([]byte(info.Reason), []byte("全黑")) {
			t.Skipf("显示器休眠/未授权，本机无法产出真实画面：%s", info.Reason)
		}
		t.Fatalf("真机 cgo 抓屏应可用，实际不可用：reason=%q hint=%q", info.Reason, info.Hint)
	}

	const rounds = 20
	opt := GrabOptions{MaxWidth: 1280, Quality: 60}

	// 先抓一帧热身（首次调用会触发 CoreGraphics 的懒初始化）。
	frame, err := cap.Grab(context.Background(), opt)
	if err != nil {
		t.Fatalf("热身抓屏失败: %v", err)
	}
	if frame.Width != 1280 {
		t.Fatalf("MaxWidth=1280 未生效，实际宽 %d", frame.Width)
	}
	// 高度必须按同比例缩（1920×1080 → 1280×720），不能只缩宽。
	if frame.Width == 1280 && frame.Height == 1080 {
		t.Fatalf("只缩了宽没缩高：%d×%d", frame.Width, frame.Height)
	}

	var total time.Duration
	worst := time.Duration(0)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		f, err := cap.Grab(context.Background(), opt)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("第 %d 次抓屏失败: %v", i+1, err)
		}
		if f.Width <= 0 || f.Height <= 0 {
			t.Fatalf("第 %d 帧尺寸非法: %d×%d", i+1, f.Width, f.Height)
		}
		if _, err := jpeg.Decode(bytes.NewReader(f.JPEG)); err != nil {
			t.Fatalf("第 %d 帧不是合法 JPEG: %v", i+1, err)
		}
		total += elapsed
		if elapsed > worst {
			worst = elapsed
		}
	}

	avg := total / rounds
	screen := Screen{Width: 0, Height: 0}
	if ss, ok := cap.(screenSource); ok {
		screen = ss.ScreenSize()
	}
	t.Logf("cgo 抓屏实测：平均 %s/帧（%d 次），最慢 %s，屏幕 %d×%d，编码 %d×%d",
		avg.Round(time.Millisecond), rounds, worst.Round(time.Millisecond),
		screen.Width, screen.Height, frame.Width, frame.Height)
	t.Logf("理论上限 %.1f fps；同机 screencapture 实测 110–140ms/帧（~5fps）",
		float64(time.Second)/float64(avg))

	if raceEnabled {
		// -race 下每次内存访问都被插桩，这个数字不代表真实帧率，只在日志里留档。
		t.Logf("注意：本用例跑在 -race 下，上面的耗时被插桩放大了，80ms 门槛已跳过")
		return
	}
	if avg > 80*time.Millisecond {
		t.Fatalf("平均 %s/帧 超过 80ms 门槛（屏幕休眠？先用 caffeinate -u 唤醒）", avg.Round(time.Millisecond))
	}
	if fps := float64(time.Second) / float64(avg); fps < 15 {
		t.Fatalf("实测 %.1f fps，没到 15fps 的验收线", fps)
	}
}

// newCGDisplayCapture 在符号不可用时必须报错，让 defaultCapture 能回退。
func TestCGOBackendAvailable(t *testing.T) {
	cap, err := newCGDisplayCapture(Config{})
	if err != nil {
		t.Skipf("本机 cgo 抓屏不可用（%v），回退路径生效", err)
	}
	defer cap.Close()
	if got := cap.Info().Backend; got != "cgo-cgwindow" {
		t.Fatalf("Info().Backend 应为 cgo-cgwindow，实际 %q", got)
	}
	if !cap.(fastGrabber).fastGrab() {
		t.Fatal("cgo 后端必须标记为快后端（帧流据此放宽 MaxFPS）")
	}
}
