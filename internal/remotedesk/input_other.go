//go:build !darwin && !linux

package remotedesk

// 其他平台：键鼠注入不做。Capability 老实说不可用，前端据此只显示画面。
func newPlatformInput(cfg Config) (Input, error) {
	_ = cfg
	return unavailableInput{Capability{
		Backend: "none",
		Reason:  "当前平台不支持键鼠注入",
	}}, nil
}
