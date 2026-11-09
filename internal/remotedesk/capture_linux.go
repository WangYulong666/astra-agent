//go:build linux

package remotedesk

import (
	"os"
	"os/exec"
	"strings"
)

// Linux：依次探测 gnome-screenshot / scrot / import（ImageMagick），
// 用第一个能真出图的。三者的「存到文件」参数各不相同。
//
// 顺序有讲究：gnome-screenshot 在 Wayland 下可用（走桌面 portal，首次可能
// 要用户点一下系统弹窗），scrot/import 是老 X11 路子。
var linuxShotTools = []struct {
	name string
	args func(path string) []string
}{
	{"gnome-screenshot", func(p string) []string { return []string{"-f", p} }},
	{"scrot", func(p string) []string { return []string{p} }},
	{"import", func(p string) []string { return []string{"-window", "root", p} }},
}

func newPlatformCapture(cfg Config) (Capture, error) {
	var last Capability
	for _, tool := range linuxShotTools {
		bin, err := exec.LookPath(tool.name)
		if err != nil {
			continue
		}
		c := &commandCapture{backend: tool.name, binPath: bin, args: tool.args}
		// 真抓一帧来验证（只读，不改屏幕状态）。「装了但跑不通」很常见
		// （没有 DISPLAY、Wayland 限制、缺依赖），必须实测。
		cap := c.probeLocked()
		if cap.Available {
			if cfg.Logger != nil {
				cfg.Logger.Printf("remotedesk: 抓屏后端选用 %s", tool.name)
			}
			return c, nil
		}
		last = cap
	}
	if last.Reason == "" {
		last = Capability{
			Backend: "none",
			Reason:  "未找到可用的抓屏工具（gnome-screenshot / scrot / import 都不存在）",
			Hint:    "apt install gnome-screenshot 或 scrot",
		}
	}
	if h := linuxScreenHint(); h != "" {
		last.Hint = strings.TrimSpace(last.Hint + "；" + h)
	}
	return unavailableCapture{last}, nil
}

// linuxScreenHint 在探测失败时补一句环境说明（Wayland 下的经典坑）。
func linuxScreenHint() string {
	if strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != "" {
		return "当前是 Wayland 会话：X11 工具（scrot/import）不可用，请用 gnome-screenshot"
	}
	return ""
}
