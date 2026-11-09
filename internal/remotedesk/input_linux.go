//go:build linux

package remotedesk

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Linux 键鼠注入：xdotool（X11）。
//
// Wayland 下 xdotool 基本无效（拿不到全局输入注入权限），所以：
//   - 没装 xdotool 且是 Wayland → 直接报不可用，Reason 写清原因；
//   - 装了 xdotool 也不代表 Wayland 能用，Apply 失败时把 stderr 带出去。
//
// 所有值都通过 exec.Command 的 argv 传，参数里的空格/分号不会被 shell 解释；
// 文本用 `--` 终止选项解析，避免以 "-" 开头的文本被当成 xdotool 选项。

var xdotoolKeys = map[string]string{
	"Enter": "Return", "Tab": "Tab", "Escape": "Escape", "Backspace": "BackSpace",
	"Delete": "Delete", "Space": "space", "ArrowUp": "Up", "ArrowDown": "Down",
	"ArrowLeft": "Left", "ArrowRight": "Right", "Home": "Home", "End": "End",
	"PageUp": "Prior", "PageDown": "Next",
}

var xdotoolMods = map[string]string{
	"cmd": "super", "ctrl": "ctrl", "alt": "alt", "shift": "shift",
}

type xdotoolInput struct {
	mu     sync.Mutex
	bin    string
	caps   capabilityCache
	closed bool
}

func newPlatformInput(cfg Config) (Input, error) {
	bin, _ := exec.LookPath("xdotool") // 找不到就是空串，由 probe 解释原因
	_ = cfg
	return &xdotoolInput{bin: bin, caps: capabilityCache{ttl: probeCacheTTL}}, nil
}

func (x *xdotoolInput) Info() Capability { return x.caps.get(x.probe) }

func (x *xdotoolInput) probe() Capability {
	if x.bin == "" {
		if strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != "" {
			return Capability{
				Backend: "none",
				Reason:  "Wayland 会话下没有可用的键鼠注入后端（xdotool 只能驱动 X11）",
				Hint:    "改用 X11 会话；或在 Wayland 下配置 ydotool 后重试（本轮未接入）",
			}
		}
		return Capability{
			Backend: "none",
			Reason:  "未安装 xdotool",
			Hint:    "apt install xdotool",
		}
	}
	// 探测不改屏幕状态：只要二进制存在就认为可用，真正的失败留给 Apply。
	return Capability{Available: true, Backend: "xdotool"}
}

func (x *xdotoolInput) Close() error {
	x.mu.Lock()
	x.closed = true
	x.mu.Unlock()
	return nil
}

func (x *xdotoolInput) Apply(ctx context.Context, ev InputEvent, screen Screen) error {
	x.mu.Lock()
	closed := x.closed
	x.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: 注入后端已关闭", ErrUnavailable)
	}
	if cap := x.Info(); !cap.Available {
		return fmt.Errorf("%w: %s", ErrUnavailable, cap.Reason)
	}
	if !validOps[ev.Op] {
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}
	mods, err := parseMods(ev.Mods)
	if err != nil {
		return err
	}
	px, py := pixelXY(ev.X, ev.Y, screen)

	ctx, cancel := context.WithTimeout(ctx, inputApplyTimeout)
	defer cancel()

	var args []string
	switch ev.Op {
	case "move":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py)}
	case "down":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py), "mousedown", "1"}
	case "up":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py), "mouseup", "1"}
	case "click":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py), "click", "1"}
	case "dblclick":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py), "click", "--repeat", "2", "--delay", "80", "1"}
	case "rightclick":
		args = []string{"mousemove", strconv.Itoa(px), strconv.Itoa(py), "click", "3"}
	case "wheel":
		steps, down := wheelSteps(ev.DY)
		if steps == 0 {
			return nil
		}
		button := "4" // 4 = 向上（滚轮上推）
		if down {
			button = "5"
		}
		args = []string{"click", "--repeat", strconv.Itoa(steps), button}
	case "text":
		if ev.Text == "" {
			return nil
		}
		// `--` 之后的第一个参数即使以 "-" 开头也不会被当成选项。
		args = []string{"type", "--delay", "12", "--", ev.Text}
	case "key":
		key, ok := xdotoolKeyName(ev.Key)
		if !ok {
			return fmt.Errorf("%w: 非法键名 %q", ErrDenied, ev.Key)
		}
		combo := key
		if len(mods) > 0 {
			parts := make([]string, 0, len(mods)+1)
			for _, m := range mods {
				parts = append(parts, xdotoolMods[m])
			}
			combo = strings.Join(append(parts, key), "+")
		}
		args = []string{"key", "--clearmodifiers", combo}
	default:
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}
	return x.run(ctx, args...)
}

func (x *xdotoolInput) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, x.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := clipReason(stderr.String())
		if msg == "" {
			msg = clipReason(err.Error())
		}
		if strings.Contains(msg, "XGetInputFocus") || strings.Contains(msg, "Cannot open display") ||
			strings.Contains(msg, "WAYLAND") {
			return fmt.Errorf("%w: xdotool 无法作用到当前会话: %s", ErrUnavailable, msg)
		}
		return fmt.Errorf("remotedesk: xdotool 执行失败: %s", msg)
	}
	return nil
}

func xdotoolKeyName(key string) (string, bool) {
	if len(key) == 1 && validKeyName(key) {
		return key, true
	}
	name, ok := xdotoolKeys[key]
	return name, ok
}
