package remotedesk

import "fmt"

// 平台默认抓屏后端。由 R2 实现（macOS: screencapture + Go 重编码；Linux: gnome-screenshot/scrot/import）。
func defaultCapture(cfg Config) (Capture, error) {
	_ = cfg
	return nil, fmt.Errorf("remotedesk: 抓屏后端未接入")
}
