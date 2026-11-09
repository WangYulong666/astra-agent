package remotedesk

import "fmt"

// 平台默认注入后端。由 R2 实现（macOS: cliclick 或 osascript；Linux: xdotool）。
func defaultInput(cfg Config) (Input, error) {
	_ = cfg
	return nil, fmt.Errorf("remotedesk: 注入后端未接入")
}
