//go:build !darwin && !linux

package remotedesk

// 其他平台：不假装支持。Capabilities 会老实报 Available=false，
// 上层据此把「监看」整层标成不可用，只留 L2 快捷操作（如果该平台有）。
func newPlatformCapture(cfg Config) (Capture, error) {
	_ = cfg
	return unavailableCapture{Capability{
		Backend: "none",
		Reason:  "当前平台不支持抓屏",
	}}, nil
}
