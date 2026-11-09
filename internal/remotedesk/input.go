package remotedesk

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// ── 输入后端骨架 ────────────────────────────────────────────────
//
// 客户端发来的坐标是**归一化 0..1**（相对编码后的画面），注入时必须乘真实
// 屏幕尺寸再四舍五入——否则手机端换个画质档位，点击位置就整体偏移。
//
// 安全底线（每一层都要守住）：
//  1. Op / Key / Mods 一律白名单，绝不「原样透传」给外部命令；
//  2. Text 长度上限 2000 字节（按 rune 边界截断），超了记日志；
//  3. 任何用户数据都**不进 shell 字符串**——所有后端都用 exec.Command
//     直接给 argv（不经 sh -c），osascript 的文本经 argv 传给脚本的 run
//     处理器（见 input_darwin.go 的注释）。

const (
	// maxTextBytes 是单次 text 注入的上限。虚拟键盘一次最多也就几百字，
	// 2000 足够；再大基本是拿它当传输通道用，属于滥用。
	maxTextBytes = 2000
	// maxWheelSteps 是单次滚轮事件的最大格数，防止一帧滚到底。
	maxWheelSteps = 50
	// inputApplyTimeout 是单次注入的兜底超时。
	inputApplyTimeout = 5 * time.Second
)

// defaultInput 按 GOOS 分派到平台实现。
func defaultInput(cfg Config) (Input, error) { return newPlatformInput(cfg) }

// validOps 是契约里列的 9 种操作，多一个都不认。
var validOps = map[string]bool{
	"move":       true,
	"down":       true,
	"up":         true,
	"click":      true,
	"dblclick":   true,
	"rightclick": true,
	"wheel":      true,
	"key":        true,
	"text":       true,
}

// allowedNamedKeys 是 Key 字段允许的命名键（其余只允许单个 [A-Za-z0-9]）。
var allowedNamedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Escape": true, "Backspace": true, "Delete": true,
	"Space": true, "ArrowUp": true, "ArrowDown": true, "ArrowLeft": true,
	"ArrowRight": true, "Home": true, "End": true, "PageUp": true, "PageDown": true,
}

var allowedMods = map[string]bool{"cmd": true, "ctrl": true, "alt": true, "shift": true}

// sanitizeEvent 做白名单校验与归一化，返回 error 表示这条事件必须丢弃。
// 校验通过后内容可能被就地修正（文本截断、坐标夹紧、mods 规范化）。
func sanitizeEvent(ev *InputEvent, lg *log.Logger) error {
	if !validOps[ev.Op] {
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}

	if len(ev.Text) > maxTextBytes {
		b := []byte(ev.Text)[:maxTextBytes]
		// 按 rune 边界回退，别把中文/emoji 截成半个
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
		ev.Text = string(b)
		if lg != nil {
			lg.Printf("remotedesk: text 超过 %d 字节，已截断", maxTextBytes)
		}
	}

	if ev.Op == "key" && ev.Key == "" {
		return fmt.Errorf("%w: key 事件缺少键名", ErrDenied)
	}
	if ev.Key != "" && !validKeyName(ev.Key) {
		return fmt.Errorf("%w: 非法键名 %q", ErrDenied, ev.Key)
	}

	mods, err := parseMods(ev.Mods)
	if err != nil {
		return err
	}
	ev.Mods = strings.Join(mods, ",")

	ev.X = clamp01(ev.X)
	ev.Y = clamp01(ev.Y)
	if ev.Op == "wheel" {
		ev.DX = clampInt(ev.DX, -maxWheelSteps, maxWheelSteps)
		ev.DY = clampInt(ev.DY, -maxWheelSteps, maxWheelSteps)
	}
	return nil
}

// validKeyName：单字符只允许 [A-Za-z0-9]，其余必须是命名键白名单。
func validKeyName(k string) bool {
	if len(k) == 1 {
		c := k[0]
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	return allowedNamedKeys[k]
}

// parseMods 校验 "cmd,shift" 这类组合，去重并保持固定顺序（便于测试与日志）。
func parseMods(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		if !allowedMods[m] {
			return nil, fmt.Errorf("%w: 非法修饰键 %q", ErrDenied, m)
		}
		seen[m] = true
	}
	var out []string
	for _, m := range []string{"cmd", "ctrl", "alt", "shift"} {
		if seen[m] {
			out = append(out, m)
		}
	}
	return out, nil
}

// pixelXY 把归一化坐标换算成像素坐标（四舍五入，并夹到屏幕范围内）。
func pixelXY(x, y float64, screen Screen) (int, int) {
	if screen.Width <= 0 || screen.Height <= 0 {
		return 0, 0
	}
	px := int(math.Round(clamp01(x) * float64(screen.Width)))
	py := int(math.Round(clamp01(y) * float64(screen.Height)))
	px = clampInt(px, 0, screen.Width-1)
	py = clampInt(py, 0, screen.Height-1)
	return px, py
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// wheelSteps 把 dx/dy 换成「滚几格、往哪个方向」。
// dy>0 是向下滚（触屏手势的自然直觉），一格按 40 个单位算，1..maxWheelSteps。
func wheelSteps(dy int) (steps int, down bool) {
	if dy == 0 {
		return 0, false
	}
	n := int(math.Round(math.Abs(float64(dy)) / 40))
	if n < 1 {
		n = 1
	}
	n = clampInt(n, 1, maxWheelSteps)
	return n, dy > 0
}

// unavailableInput 是「本机没有注入能力」的占位实现。
// Info() 老实说不可用，Apply 返回 ErrUnavailable —— 上层据此提示用户去开权限。
type unavailableInput struct{ cap Capability }

func (u unavailableInput) Info() Capability { return u.cap }

func (u unavailableInput) Apply(context.Context, InputEvent, Screen) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, u.cap.Reason)
}

func (u unavailableInput) Close() error { return nil }
