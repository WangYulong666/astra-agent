package remotedesk

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeEventRejects(t *testing.T) {
	cases := []struct {
		name string
		ev   InputEvent
	}{
		{"非法 Op", InputEvent{Op: "explode"}},
		{"空 Op", InputEvent{}},
		{"大小写不符的 Op", InputEvent{Op: "Click"}},
		{"非法 Key（带空格）", InputEvent{Op: "key", Key: "rm -rf /"}},
		{"非法 Key（多字符）", InputEvent{Op: "key", Key: "ab"}},
		{"非法 Key（注入味道）", InputEvent{Op: "key", Key: `a"; do shell script "x`}},
		{"key 事件缺键名", InputEvent{Op: "key"}},
		{"非法 Mods", InputEvent{Op: "key", Key: "a", Mods: "cmd;rm"}},
		{"不在白名单的 Mods", InputEvent{Op: "click", X: 0.5, Y: 0.5, Mods: "hyper"}},
	}
	for _, c := range cases {
		ev := c.ev
		if err := sanitizeEvent(&ev, nil); err == nil {
			t.Errorf("%s：应被拒绝，实际通过", c.name)
		} else if !errors.Is(err, ErrDenied) {
			t.Errorf("%s：应包装 ErrDenied，实际 %v", c.name, err)
		}
	}
}

func TestSanitizeEventAcceptsContractOps(t *testing.T) {
	ops := []InputEvent{
		{Op: "move", X: 0.5, Y: 0.5},
		{Op: "down", X: 0.2, Y: 0.3},
		{Op: "up", X: 0.2, Y: 0.3},
		{Op: "click", X: 0.1, Y: 0.9},
		{Op: "dblclick", X: 0.1, Y: 0.9},
		{Op: "rightclick", X: 0.1, Y: 0.9},
		{Op: "wheel", DX: 0, DY: -120},
		{Op: "key", Key: "Enter"},
		{Op: "key", Key: "a", Mods: "cmd,shift"},
		{Op: "text", Text: "hello 世界"},
	}
	for _, ev := range ops {
		e := ev
		if err := sanitizeEvent(&e, nil); err != nil {
			t.Errorf("%s 应被接受，实际 %v", e.Op, err)
		}
	}
}

func TestSanitizeTextTruncates(t *testing.T) {
	ev := InputEvent{Op: "text", Text: strings.Repeat("a", maxTextBytes+500)}
	if err := sanitizeEvent(&ev, nil); err != nil {
		t.Fatalf("超长文本应截断而不是拒绝: %v", err)
	}
	if len(ev.Text) != maxTextBytes {
		t.Fatalf("应截断到 %d 字节，实际 %d", maxTextBytes, len(ev.Text))
	}

	// 中文按 3 字节算，截断点必须回退到 rune 边界，不能切出半个字。
	ev = InputEvent{Op: "text", Text: strings.Repeat("中", maxTextBytes)}
	if err := sanitizeEvent(&ev, nil); err != nil {
		t.Fatalf("中文超长应截断: %v", err)
	}
	if !utf8.ValidString(ev.Text) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if len(ev.Text) > maxTextBytes {
		t.Fatalf("截断后仍超长：%d", len(ev.Text))
	}
}

func TestSanitizeClampsCoordinates(t *testing.T) {
	ev := InputEvent{Op: "click", X: -3, Y: 9}
	if err := sanitizeEvent(&ev, nil); err != nil {
		t.Fatalf("sanitizeEvent: %v", err)
	}
	if ev.X != 0 || ev.Y != 1 {
		t.Fatalf("坐标应夹到 0..1，实际 %f,%f", ev.X, ev.Y)
	}
}

func TestParseMods(t *testing.T) {
	mods, err := parseMods(" shift, CMD ,cmd")
	if err != nil {
		t.Fatalf("parseMods: %v", err)
	}
	if got := strings.Join(mods, ","); got != "cmd,shift" {
		t.Fatalf("应去重并按固定顺序输出，实际 %q", got)
	}
	if m, err := parseMods(""); err != nil || len(m) != 0 {
		t.Fatalf("空串应得到空结果，实际 %v %v", m, err)
	}
}

func TestPixelXY(t *testing.T) {
	screen := Screen{Width: 1920, Height: 1080}
	cases := []struct {
		x, y  float64
		wantX int
		wantY int
	}{
		{0.5, 0.5, 960, 540},
		{0, 0, 0, 0},
		{1, 1, 1919, 1079}, // 夹到屏幕内，不能越界
		{2, -1, 1919, 0},
	}
	for _, c := range cases {
		x, y := pixelXY(c.x, c.y, screen)
		if x != c.wantX || y != c.wantY {
			t.Errorf("pixelXY(%f,%f) = %d,%d，期望 %d,%d", c.x, c.y, x, y, c.wantX, c.wantY)
		}
	}
	// 零尺寸屏幕不能算出负数或 panic
	if x, y := pixelXY(0.5, 0.5, Screen{}); x != 0 || y != 0 {
		t.Fatalf("零尺寸屏幕应返回 0,0，实际 %d,%d", x, y)
	}
}

func TestWheelSteps(t *testing.T) {
	if n, down := wheelSteps(0); n != 0 || down {
		t.Fatalf("dy=0 应不滚，实际 %d %v", n, down)
	}
	if n, down := wheelSteps(120); n != 3 || !down {
		t.Fatalf("dy=120 应向下滚 3 格，实际 %d %v", n, down)
	}
	if n, down := wheelSteps(-40); n != 1 || down {
		t.Fatalf("dy=-40 应向上滚 1 格，实际 %d %v", n, down)
	}
	if n, _ := wheelSteps(999999); n != maxWheelSteps {
		t.Fatalf("应夹到 %d 格，实际 %d", maxWheelSteps, n)
	}
}

func TestUnavailableInput(t *testing.T) {
	in := NewUnavailableInput()
	cap := in.Info()
	if cap.Available {
		t.Fatal("不该报可用")
	}
	if cap.Reason != "permission_denied" {
		t.Fatalf("Reason 应为 permission_denied，实际 %q", cap.Reason)
	}
	if cap.Hint == "" {
		t.Fatal("Hint 应指到辅助功能设置")
	}
	err := in.Apply(nil, InputEvent{Op: "click", X: 0.5, Y: 0.5}, Screen{Width: 100, Height: 100})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Apply 应返回 ErrUnavailable，实际 %v", err)
	}
}

func TestFakeInputRecords(t *testing.T) {
	in := NewFakeInput()
	screen := Screen{Width: 1920, Height: 1080}
	for i := 0; i < 3; i++ {
		if err := in.Apply(nil, InputEvent{Op: "move", X: 0.5, Y: 0.5}, screen); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if in.EventCount() != 3 {
		t.Fatalf("应记录 3 条，实际 %d", in.EventCount())
	}
	if s := in.Screens()[0]; s != screen {
		t.Fatalf("应记录屏幕尺寸，实际 %+v", s)
	}
}

// 真机用例：本机 L3 到底能不能控。
// 断言的是**降级路径**：不可用时必须明确报 ErrUnavailable（而不是静默成功）。
// 如果机器上有 cliclick 且已授权，就跳过——不敢在真机上真的敲键盘。
func TestRealInputCapability(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("非 darwin 平台，真机注入探测不适用")
	}
	in, err := defaultInput(Config{})
	if err != nil {
		t.Fatalf("defaultInput: %v", err)
	}
	defer in.Close()

	cap := in.Info()
	t.Logf("Info: available=%v backend=%q reason=%q hint=%q",
		cap.Available, cap.Backend, cap.Reason, cap.Hint)
	if cap.Available {
		t.Skipf("本机已具备注入能力（backend=%s），跳过权限缺失路径断言", cap.Backend)
	}
	if cap.Reason == "" {
		t.Fatal("不可用时必须写清 Reason")
	}
	err = in.Apply(context.Background(), InputEvent{Op: "key", Key: "a"}, Screen{Width: 1920, Height: 1080})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("不可用时 Apply 应返回 ErrUnavailable，实际 %v", err)
	}
	t.Logf("Apply 正确降级：%v", err)
}
