//go:build darwin

package remotedesk

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// macOS 键鼠注入。两条路，按「有没有 cliclick」二选一：
//
//	cliclick（用户自装，brew install cliclick）—— 能移动/单击/双击/右键/拖拽
//	osascript + System Events —— 系统自带，但能力弱：不能移动光标、不能拖拽、
//	                              不能右键、不能滚轮；只能点击/按键/输入文本
//
// 两者都需要「辅助功能」TCC 授权（System Events 的 -10004 就是没授权）。
//
// ── 防注入（重点）────────────────────────────────────────────────
// 用户数据（文本、键名）一律**不进脚本源码**，而是作为 argv 传给 osascript，
// 由脚本的 run 处理器从 item 1 of argv 取。这是比「strconv.Quote 拼 AppleScript
// 字面量」更彻底的做法：
//   - 引号、反斜杠、换行、`do shell script` 的拼串都不再是问题，因为文本
//     从头到尾没有出现在脚本里；
//   - 脚本模板是常量，cmd 也是常量，不存在任何拼接点。
// 唯一会动态生成的脚本片段是修饰键，但它的取值只来自
// cmd/ctrl/alt/shift → command/control/option/shift 这个固定映射表，
// 等价于枚举，不含用户数据。
// 命令一律用 exec.Command 直接给 argv，从不经过 sh -c，所以 shell 元字符
// （; | $() ` 等）也没有任何被解释的机会。

const (
	asUIEnabledScript = `tell application "System Events" to get UI elements enabled`

	// 文本：整段从 argv 取，转义问题彻底消失。
	asTypeScript = `on run argv
	tell application "System Events" to keystroke (item 1 of argv)
end run`

	// 命名键：key code 是数字，坐标也是数字，都从 argv 取。
	asKeyCodeScript = `on run argv
	set code to (item 1 of argv) as integer
	tell application "System Events" to key code code
end run`

	asClickScript = `on run argv
	set px to (item 1 of argv) as integer
	set py to (item 2 of argv) as integer
	tell application "System Events"
		click at {px, py}
	end tell
end run`
)

// asModifierName 是固定映射（枚举，不是用户输入）。
var asModifierName = map[string]string{
	"cmd":   "command",
	"ctrl":  "control",
	"alt":   "option",
	"shift": "shift",
}

// asKeyCodes 是命名键 → macOS 虚拟键码。
var asKeyCodes = map[string]int{
	"Enter": 36, "Tab": 48, "Escape": 53, "Backspace": 51, "Delete": 117,
	"Space": 49, "ArrowLeft": 123, "ArrowRight": 124, "ArrowDown": 125, "ArrowUp": 126,
	"Home": 115, "End": 119, "PageUp": 116, "PageDown": 121,
}

// cliclickKeys 是命名键 → cliclick 的键名。
var cliclickKeys = map[string]string{
	"Enter": "enter", "Tab": "tab", "Escape": "esc", "Backspace": "backspace",
	"Delete": "delete", "Space": "space", "ArrowUp": "arrow-up", "ArrowDown": "arrow-down",
	"ArrowLeft": "arrow-left", "ArrowRight": "arrow-right", "Home": "home", "End": "end",
	"PageUp": "page-up", "PageDown": "page-down",
}

type darwinInput struct {
	mu          sync.Mutex
	cliclick    string
	hasCliclick bool
	caps        capabilityCache
	closed      bool

	// 拖拽状态：down 记下起点，up 时把「起点→终点」一次 dd: 出去。
	dragging     bool
	downX, downY int
}

func newPlatformInput(cfg Config) (Input, error) {
	_ = cfg
	d := &darwinInput{}
	if p, err := exec.LookPath("cliclick"); err == nil {
		d.cliclick, d.hasCliclick = p, true
	}
	return d, nil
}

// Info 探测走缓存（30s）：TCC 授权改了之后不用重启服务。
// 探测本身**绝不产生副作用**——只用 cliclick 是否存在 / 只读的
// `get UI elements enabled` 来判断，不会去真的点一下试试。
func (d *darwinInput) Info() Capability { return d.caps.get(d.probe) }

func (d *darwinInput) probe() Capability {
	if d.hasCliclick {
		// cliclick 存在就直接认为可用；真按下去被 TCC 拦的话，
		// Apply 会把 stderr 带回来（见 applyCliclick）。
		return Capability{Available: true, Backend: "cliclick", Hint: hintAccessibility}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := runOsascript(ctx, asUIEnabledScript)
	if err != nil {
		if isPermissionErr(err.Error()) {
			return Capability{
				Backend: "osascript",
				Reason:  "permission_denied",
				Hint:    hintAccessibility + "；" + hintInstallCliclick,
			}
		}
		return Capability{Backend: "osascript", Reason: clipReason(err.Error()), Hint: hintAccessibility}
	}
	if strings.EqualFold(strings.TrimSpace(out), "true") {
		return Capability{
			Backend: "osascript",
			Hint:    "未装 cliclick：拖动/右键/滚轮不可用；" + hintInstallCliclick,
		}
	}
	return Capability{Backend: "osascript", Reason: "permission_denied", Hint: hintAccessibility}
}

func (d *darwinInput) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

func (d *darwinInput) Apply(ctx context.Context, ev InputEvent, screen Screen) error {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: 注入后端已关闭", ErrUnavailable)
	}
	if cap := d.Info(); !cap.Available {
		return fmt.Errorf("%w: %s", ErrUnavailable, cap.Reason)
	}
	// 后端自己的白名单兜底：即使调用方漏了 sanitize，也不会把任意字符串
	// 拼进 cliclick 参数或 AppleScript 里。
	if !validOps[ev.Op] {
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}
	if _, err := parseMods(ev.Mods); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, inputApplyTimeout)
	defer cancel()
	if d.hasCliclick {
		return d.applyCliclick(ctx, ev, screen)
	}
	return d.applyOsascript(ctx, ev, screen)
}

func (d *darwinInput) applyCliclick(ctx context.Context, ev InputEvent, screen Screen) error {
	px, py := pixelXY(ev.X, ev.Y, screen)
	var args []string
	switch ev.Op {
	case "move":
		args = append(args, fmt.Sprintf("m:%d,%d", px, py))
	case "click":
		args = append(args, fmt.Sprintf("c:%d,%d", px, py))
	case "dblclick":
		args = append(args, fmt.Sprintf("dc:%d,%d", px, py))
	case "rightclick":
		args = append(args, fmt.Sprintf("rc:%d,%d", px, py))
	case "down":
		d.mu.Lock()
		d.dragging, d.downX, d.downY = true, px, py
		d.mu.Unlock()
		args = append(args, fmt.Sprintf("m:%d,%d", px, py))
	case "up":
		d.mu.Lock()
		dragging, sx, sy := d.dragging, d.downX, d.downY
		d.dragging = false
		d.mu.Unlock()
		if dragging {
			// cliclick 没有单独的 mouse down/up。dd: 的语义是「在当前位置按下、
			// 拖到目标、松手」，所以先 m: 回到按下点，再 dd: 到抬起点，
			// 一次调用完成整段拖拽（中间那些 move 只是让光标跟着走）。
			args = append(args, fmt.Sprintf("m:%d,%d", sx, sy), fmt.Sprintf("dd:%d,%d", px, py))
		} else {
			args = append(args, fmt.Sprintf("c:%d,%d", px, py))
		}
	case "wheel":
		return fmt.Errorf("%w: macOS 无系统级滚轮注入（cliclick 也不支持 scroll），这一路先不做", ErrUnavailable)
	case "key":
		key, ok := cliclickKeyName(ev.Key)
		if !ok {
			return fmt.Errorf("%w: 非法键名 %q", ErrDenied, ev.Key)
		}
		mods, _ := parseMods(ev.Mods)
		args = append(args, "kp:"+strings.Join(append(mods, key), "+"))
	case "text":
		if ev.Text == "" {
			return nil
		}
		args = append(args, "t:"+ev.Text)
	default:
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}
	if len(args) == 0 {
		return nil
	}
	return d.runCliclick(ctx, args...)
}

func (d *darwinInput) applyOsascript(ctx context.Context, ev InputEvent, screen Screen) error {
	px, py := pixelXY(ev.X, ev.Y, screen)
	switch ev.Op {
	case "click", "dblclick":
		// AppleScript 没有「双击」原语，连按两次等价（间隔靠脚本自身执行速度）。
		n := 1
		if ev.Op == "dblclick" {
			n = 2
		}
		for i := 0; i < n; i++ {
			if err := runOsascriptVoid(ctx, asClickScript, strconv.Itoa(px), strconv.Itoa(py)); err != nil {
				return err
			}
		}
		return nil
	case "rightclick":
		return fmt.Errorf("%w: osascript 后端不支持右键，安装 cliclick 后可用", ErrUnavailable)
	case "wheel":
		return fmt.Errorf("%w: osascript 后端不支持滚轮（System Events 没有 scroll 原语）", ErrUnavailable)
	case "move", "down", "up":
		// System Events 没有「移动光标」「按下/抬起」的原语。这里退化成无操作，
		// 而不是每条事件都报错——否则一次拖动会刷出上百条错误。
		// 用户看到的是能力提示（Info().Hint）里写的「拖动不可用，装 cliclick」。
		return nil
	case "text":
		if ev.Text == "" {
			return nil
		}
		return runOsascriptVoid(ctx, asTypeScript, ev.Text)
	case "key":
		mods, _ := parseMods(ev.Mods)
		if code, ok := asKeyCodes[ev.Key]; ok {
			return runOsascriptVoid(ctx, asModifiedScript(mods, "key code code"), strconv.Itoa(code))
		}
		if !validKeyName(ev.Key) {
			return fmt.Errorf("%w: 非法键名 %q", ErrDenied, ev.Key)
		}
		// 单字符原样交给 keystroke（保留大小写），同样走 argv。
		return runOsascriptVoid(ctx, asModifiedScript(mods, "keystroke (item 1 of argv)"), ev.Key)
	default:
		return fmt.Errorf("%w: 非法操作 %q", ErrDenied, ev.Op)
	}
}

// asModifiedScript 用 key down/up 包住脚本体，实现「cmd+a」这类组合键。
// 拼进去的只有 asModifierName 表里的固定枚举值，不含用户数据。
func asModifiedScript(mods []string, body string) string {
	var b strings.Builder
	b.WriteString("on run argv\n\ttell application \"System Events\"\n")
	for _, m := range mods {
		if name, ok := asModifierName[m]; ok {
			fmt.Fprintf(&b, "\t\tkey down {%s}\n", name)
		}
	}
	b.WriteString("\t\t" + body + "\n")
	for _, m := range mods {
		if name, ok := asModifierName[m]; ok {
			fmt.Fprintf(&b, "\t\tkey up {%s}\n", name)
		}
	}
	b.WriteString("\tend tell\nend run")
	return b.String()
}

func (d *darwinInput) runCliclick(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, d.cliclick, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := clipReason(stderr.String())
		if msg == "" {
			msg = clipReason(err.Error())
		}
		// 装是装了但 TCC 没授权：必须回 ErrUnavailable 并把 stderr 带出去，
		// 让用户知道该去「辅助功能」里勾选，而不是以为指令发错了。
		if isPermissionErr(msg) {
			return fmt.Errorf("%w: cliclick 被系统拒绝: %s", ErrUnavailable, msg)
		}
		return fmt.Errorf("remotedesk: cliclick 执行失败: %s", msg)
	}
	return nil
}

// runOsascript 跑一段脚本，args 经 `--` 作为 argv 交给 run 处理器。
func runOsascript(ctx context.Context, script string, args ...string) (string, error) {
	argv := append([]string{"-e", script}, "--")
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "osascript", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := clipReason(stderr.String())
		if msg == "" {
			msg = clipReason(err.Error())
		}
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runOsascriptVoid(ctx context.Context, script string, args ...string) error {
	_, err := runOsascript(ctx, script, args...)
	if err != nil {
		if isPermissionErr(err.Error()) {
			return fmt.Errorf("%w: osascript 被系统拒绝: %s", ErrUnavailable, err.Error())
		}
		return fmt.Errorf("remotedesk: osascript 注入失败: %s", err.Error())
	}
	return nil
}

func cliclickKeyName(key string) (string, bool) {
	if len(key) == 1 {
		if validKeyName(key) {
			return strings.ToLower(key), true
		}
		return "", false
	}
	name, ok := cliclickKeys[key]
	return name, ok
}
