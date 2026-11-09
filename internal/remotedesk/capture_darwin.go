//go:build darwin

package remotedesk

import (
	"os"
	"os/exec"
)

// macOS：screencapture 是系统自带，不需要装任何东西。
//
//	-x      不播放快门声（远程监看每秒抓几帧，响起来会疯）
//	-t png  输出 PNG，Go 端解码后重编码成 JPEG
//
// 刻意不加 -C / -R / -D：那些是「捕获光标 / 指定矩形 / 指定显示器」，
// 本轮不做多显示器，也不把光标画进画面（前端自己画指针位置）。
const screencaptureBin = "/usr/sbin/screencapture"

func newPlatformCapture(cfg Config) (Capture, error) {
	_ = cfg
	bin := screencaptureBin
	if p, err := exec.LookPath("screencapture"); err == nil {
		bin = p
	} else if _, err := os.Stat(screencaptureBin); err != nil {
		return unavailableCapture{Capability{
			Backend: "screencapture",
			Reason:  "未找到 screencapture（非标准 macOS 环境）",
			Hint:    "确认 /usr/sbin 在 PATH 中",
		}}, nil
	}
	return &commandCapture{
		backend: "screencapture",
		binPath: bin,
		args:    func(path string) []string { return []string{"-x", "-t", "png", path} },
	}, nil
}
